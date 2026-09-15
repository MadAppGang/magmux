#!/bin/sh
# prove-r4.sh: the R4 proof that lifting the socket's wire vocabulary into
# package protocol and the socket client into package client, and splitting the
# files that straddled the new boundaries, changed nothing but names, packages
# and file placement. Run from anywhere inside the repository:
#
#   sh test/reorg/prove-r4.sh PRE_R4
#
# PRE_R4 is the tree R4 started from: either a directory holding its mux/ and
# mcp/, or a git revision (the R3 commit) that has them.
#
# The group is every .go file R4 read or wrote:
#   old: PRE_R4's mux/*.go and mcp/*.go
#   new: mux/ mcp/ client/ protocol/ *.go, minus client/doc.go and
#        protocol/doc.go (checked separately: package docs only)
#
#   1. test/reorg/r4.sed (the rename map, run with perl -p) is applied to OLD.
#   2. Both sides are normalised: package clauses, import blocks and blank lines
#      dropped; the qualifiers protocol. and client. before a capital stripped,
#      so a name reads the same inside its package and out; runs of blanks
#      collapsed, since gofmt re-aligns a column when a name's length changes.
#   3. Both sides are sorted and compared as MULTISETS of lines (comm), which
#      makes the check blind to which file or position a line landed in and to
#      nothing else. Lines only in OLD print as "- ", lines only in NEW as "+ ".
#   4. That delta must equal test/reorg/r4-delta.txt exactly (its # lines are
#      commentary). The committed delta is the complete list of hand edits.
#
# Prints one PASS line and exits 0, or the unexpected delta and a FAIL line.

set -u

root=$(git rev-parse --show-toplevel 2>/dev/null) || {
	echo "FAIL: not inside the magmux git repository"
	exit 1
}
cd "$root" || exit 1

MAP=test/reorg/r4.sed
DELTA=test/reorg/r4-delta.txt
PUBLIC='client protocol'

# --delta prints the actual delta and stops, which is how r4-delta.txt's
# lines were produced; commentary is then added by hand as # lines.
print_delta=0
if [ "${1:-}" = "--delta" ]; then
	print_delta=1
	shift
fi
[ $# -eq 1 ] || {
	echo "usage: sh test/reorg/prove-r4.sh [--delta] PRE_R4_DIR_OR_REV"
	exit 2
}

tmp=$(mktemp -d "${TMPDIR:-/tmp}/prove-r4.XXXXXX") || exit 1
trap 'rm -rf "$tmp"' EXIT
trap 'exit 1' HUP INT TERM

fail() {
	echo "FAIL: $*"
	exit 1
}

# ── the old tree ─────────────────────────────────────────────────────────────
if [ -d "$1" ]; then
	old=$1
else
	mkdir "$tmp/old"
	git archive "$1" mux mcp | tar -x -C "$tmp/old" ||
		fail "cannot extract mux/ and mcp/ from revision $1"
	old=$tmp/old
fi
[ -f "$old/mux/sockrpc.go" ] && [ -f "$old/mcp/mcp_client.go" ] ||
	fail "$1 does not look like the pre-R4 tree (need mux/sockrpc.go, mcp/mcp_client.go)"

# ── check 0: the package docs are only package docs ──────────────────────────
for p in $PUBLIC; do
	f=$p/doc.go
	[ -f "$f" ] || fail "missing $f"
	awk -v p="$p" '!/^\/\// && !/^$/ && $0 != "package " p { print FILENAME ": not a package doc: " $0; bad = 1 }
		END { exit bad }' "$f" || fail "$f holds more than a package doc comment"
done

# normalise: header (package clause, import block) and blank lines out,
# qualifiers stripped, blanks collapsed.
STRIP='
BEGIN { hdr = 1; imp = 0 }
/^[ \t]*$/ { next }
hdr && imp { if ($0 ~ /^\)/) imp = 0; next }
hdr && /^package [A-Za-z_]/ { next }
hdr && /^import \(/ { imp = 1; next }
hdr && /^import / { next }
hdr && !/^\/\// { hdr = 0 }
{ print }
'
norm() {
	awk "$STRIP" | perl -pe 's/\b(protocol|client)\.(?=[A-Z])//g; s/[ \t]+/ /g; s/^ //; s/ $//'
}

# ── old side: the rename map, then normalise ─────────────────────────────────
# (File lists are built outside the pipelines: a count kept inside
# `for ... done | sort` would be lost with the subshell.)
set -- "$old"/mux/*.go "$old"/mcp/*.go
nold=$#
for f in "$@"; do
	perl -p "$MAP" "$f" | norm
done | LC_ALL=C sort >"$tmp/old.sorted"

# ── new side: normalise only ─────────────────────────────────────────────────
set --
for f in mux/*.go mcp/*.go client/*.go protocol/*.go; do
	case "$f" in client/doc.go | protocol/doc.go) ;; *) set -- "$@" "$f" ;; esac
done
nnew=$#
for f in "$@"; do
	norm <"$f"
done | LC_ALL=C sort >"$tmp/new.sorted"

# ── the delta, as multisets ──────────────────────────────────────────────────
{
	LC_ALL=C comm -23 "$tmp/old.sorted" "$tmp/new.sorted" | sed 's/^/- /'
	LC_ALL=C comm -13 "$tmp/old.sorted" "$tmp/new.sorted" | sed 's/^/+ /'
} | LC_ALL=C sort >"$tmp/got"
if [ "$print_delta" = 1 ]; then
	cat "$tmp/got"
	exit 0
fi
[ -f "$DELTA" ] || fail "missing $DELTA"
grep -v '^#' "$DELTA" | grep -v '^$' | LC_ALL=C sort >"$tmp/want"

if ! diff -u "$tmp/want" "$tmp/got" >"$tmp/diff"; then
	echo "the delta is not the committed $DELTA (--- committed, +++ actual):"
	cat "$tmp/diff"
	fail "R4 changed something the rename map and the committed delta do not account for"
fi

nlines=$(awk 'END { print NR }' "$tmp/old.sorted")
ndelta=$(awk 'END { print NR }' "$tmp/got")
echo "PASS: $nold pre-R4 files ($nlines lines, after r4.sed) and $nnew R4 files are equal as sorted line multisets except the $ndelta committed delta lines in $DELTA; doc.go files hold package docs only"
