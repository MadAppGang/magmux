#!/bin/sh
# prove-split.sh: the R2 proof that splitting mux/main.go into section files
# was a pure move. Run from anywhere inside the repository:
#
#   sh test/reorg/prove-split.sh [pristine-main.go]
#
# The source is the post-R1 mux/main.go. By default it is rebuilt from the
# baseline commit's main.go plus test/reorg/r1-main.patch (R1's only edits to
# that file), then pinned by cksum. That way the proof keeps working after R2
# deletes the file and whatever SHAs R1 and R2 are committed under. You can
# pass a path instead; it must match the same cksum.
#
# Checks, driven by test/reorg/r2-ranges.txt (`file start end` per line):
#   1. The ranges PARTITION the source: sorted, they are gapless and
#      non-overlapping and run from line 1 to the last line.
#   2. Ranges assigned to `-` hold only scaffolding (the package clause, the
#      import block and blank lines).
#   3. Each destination file equals the concatenation of its ranges in map
#      order, once package lines, import blocks and blank lines are stripped
#      from both sides. The diff must be empty.
#   4. No destination file declares `func init()`, so file order cannot change
#      init order. mux/main.go must be gone, because every line moved out of it.
#
# On success it prints one PASS line and exits 0. On failure it prints the
# offending diff or reason, then a FAIL line, and exits 1.

set -u

BASE=18460387510b378645ba99f7529678cd6d70c491
EXPECT_CKSUM='1141381794 296102'

root=$(git rev-parse --show-toplevel 2>/dev/null) || {
	echo "FAIL: not inside the magmux git repository"
	exit 1
}
cd "$root" || exit 1

MAP=test/reorg/r2-ranges.txt
PATCH=test/reorg/r1-main.patch
DEST=mux

tmp=$(mktemp -d "${TMPDIR:-/tmp}/prove-split.XXXXXX") || exit 1
trap 'rm -rf "$tmp"' EXIT
trap 'exit 1' HUP INT TERM

fail() {
	echo "FAIL: $*"
	exit 1
}

# ── the source ───────────────────────────────────────────────────────────────
if [ $# -ge 1 ]; then
	cp "$1" "$tmp/src.go" || fail "cannot read $1"
else
	git show "$BASE:main.go" >"$tmp/base.go" 2>/dev/null ||
		fail "cannot read main.go at baseline $BASE"
	patch -s -o "$tmp/src.go" "$tmp/base.go" <"$PATCH" >/dev/null 2>&1 ||
		fail "$PATCH does not apply to the baseline main.go"
fi
got=$(cksum <"$tmp/src.go" | awk '{print $1, $2}')
[ "$got" = "$EXPECT_CKSUM" ] ||
	fail "source cksum is '$got', want '$EXPECT_CKSUM' (not the post-R1 mux/main.go)"
n=$(awk 'END {print NR}' "$tmp/src.go")

# ── the map ──────────────────────────────────────────────────────────────────
awk '!/^[ \t]*#/ && NF' "$MAP" >"$tmp/map" || fail "cannot read $MAP"
awk 'NF != 3 || $2 !~ /^[0-9]+$/ || $3 !~ /^[0-9]+$/ || $2 + 0 > $3 + 0 {
	print "bad map line: " $0; bad = 1
} END { exit bad }' "$tmp/map" || fail "$MAP is malformed"

# Check 1: partition.
sort -n -k2,2 "$tmp/map" | awk -v n="$n" '
BEGIN { nx = 1 }
{
	if ($2 != nx) {
		if ($2 > nx) printf "gap: lines %d-%d are in no range\n", nx, $2 - 1
		else printf "overlap: range %s %s %s starts at %d, already covered\n", $1, $2, $3, $2
		bad = 1
	}
	if ($3 + 1 > nx) nx = $3 + 1
}
END {
	if (nx - 1 != n) { printf "ranges end at line %d, the source has %d\n", nx - 1, n; bad = 1 }
	exit bad
}' || fail "the ranges do not partition the $n-line source"

# strip: drop blank lines everywhere, and package clauses and import blocks in
# the header (before the first line that is neither a comment nor header).
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

# extract FILE: the concatenation of FILE's ranges, in map order.
extract() {
	awk -v f="$1" '$1 == f { print $2, $3 }' "$tmp/map" >"$tmp/r"
	while read -r s e; do
		sed -n "${s},${e}p" "$tmp/src.go"
	done <"$tmp/r"
}

# Check 2: scaffolding is only scaffolding.
extract - >"$tmp/scaffold"
awk '!/^[ \t]*$/ && !/^package [A-Za-z_]+$/ && !/^import \($/ && !/^\)$/ &&
	!/^\t"[^"]+"$/ && !/^import "[^"]+"$/ { print "not scaffolding: " $0; bad = 1 }
	END { exit bad }' "$tmp/scaffold" || fail "a range mapped to - holds real content"

# Check 3: every destination equals its ranges.
files=$(awk '$1 != "-" && !seen[$1]++ { print $1 }' "$tmp/map")
nfiles=0
bad=0
for f in $files; do
	nfiles=$((nfiles + 1))
	if [ ! -f "$DEST/$f" ]; then
		echo "missing: $DEST/$f"
		bad=1
		continue
	fi
	if ! grep -q '^package mux$' "$DEST/$f"; then
		echo "$DEST/$f: package clause is not 'package mux'"
		bad=1
	fi
	extract "$f" | awk "$STRIP" >"$tmp/want"
	awk "$STRIP" "$DEST/$f" >"$tmp/have"
	if ! diff -u "$tmp/want" "$tmp/have" >"$tmp/diff"; then
		echo "$DEST/$f differs from its ranges:"
		cat "$tmp/diff"
		bad=1
	fi
done
[ "$bad" = 0 ] || fail "a destination file is not its ranges"

# Check 4: no init order to disturb, and nothing left behind.
for f in $files; do
	if grep -n '^func init[[:space:]]*(' "$DEST/$f" >/dev/null; then
		fail "$DEST/$f declares func init()"
	fi
done
if grep -n '^func init[[:space:]]*(' "$tmp/src.go" >/dev/null; then
	fail "the source declares func init()"
fi
case " $(echo $files) " in
*" main.go "*) ;;
*) [ ! -e "$DEST/main.go" ] || fail "$DEST/main.go still exists, though the map moves every line out of it" ;;
esac

nranges=$(awk 'END { print NR }' "$tmp/map")
echo "PASS: $n lines of post-R1 mux/main.go partitioned by $nranges ranges; $nfiles files each equal their ranges (package/import/blank stripped); no func init()"
