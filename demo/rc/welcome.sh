#!/usr/bin/env bash
# The command the watched pane runs: a colour card, then an ordinary
# interactive shell.
#
# The card is a TEST VECTOR ON SCREEN. Every branch of the shared decoder in
# `demo/rc/frame.ts` — the 16 base colours, the 6x6x6 indexed cube, a 24-bit
# truecolor ramp, and the attribute mask — is exercised in the very first frame
# both clients receive, so a viewer checks colour fidelity by eye, on two
# surfaces, before typing anything. A decoder that gets `38;2;R;G;B` wrong is
# not an error anywhere; it is a slightly wrong picture, and this is what makes
# it a visible one.
#
# Then `exec $SHELL -i`, because the demo's whole claim is that a HUMAN types
# into this pane and both clients follow. Nothing here self-drives: output on a
# timer would make "the mirror is live" unfalsifiable, since a client that had
# stopped receiving frames would look exactly like a pane that had gone quiet.
set -u

reset='\033[0m'

cols=$(tput cols 2>/dev/null || echo 80)

rule() {
  printf '  \033[90m'
  i=0
  while [ "$i" -lt $((cols - 4)) ]; do
    printf '─'
    i=$((i + 1))
  done
  printf '%b\n' "$reset"
}

printf '\n'
printf '  \033[1mmagmux\033[0m \033[90m·\033[0m remote control demo \033[90m— this pane is being mirrored, live, to two read-only clients%b\n' "$reset"
rule

# ── the 16 base colours ─────────────────────────────────────────────────────
# 0-7 as SGR 40+n, 8-15 as SGR 100+n. Both halves are drawn because they are
# two different decoder branches on the way out (`30+n`/`90+n`) even though
# they are one indexed range on the way in.
printf '  \033[90m%-12s%b' 'base 16' "$reset"
for i in 0 1 2 3 4 5 6 7; do printf '\033[4%d;97m %d %b' "$i" "$i" "$reset"; done
for i in 0 1 2 3 4 5 6 7; do printf '\033[10%d;30m%2d %b' "$i" "$((i + 8))" "$reset"; done
printf '\n'

# ── a slice of the 6x6x6 cube, plus the greyscale ramp ──────────────────────
# Two of the six red planes, 36 swatches each: enough to show the cube is
# indexed correctly without spending six rows on it. Then 232-255, which is the
# part of the 256-colour space a wrong `38;5;N` most often lands in unnoticed.
for r in 0 3; do
  printf '  \033[90m%-12s%b' "cube r=$r" "$reset"
  for g in 0 1 2 3 4 5; do
    for b in 0 1 2 3 4 5; do
      printf '\033[48;5;%dm %b' "$((16 + 36 * r + 6 * g + b))" "$reset"
    done
  done
  printf '\n'
done
printf '  \033[90m%-12s%b' 'greyscale' "$reset"
for i in $(seq 232 255); do printf '\033[48;5;%dm %b' "$i" "$reset"; done
printf '\n'

# ── truecolor ───────────────────────────────────────────────────────────────
# A 60-step hue sweep, COMPUTED rather than tabulated, so it really is 60
# distinct `48;2;R;G;B` triples and not six repeated ones. TERM is
# screen-256color inside a pane, which is exactly why this is worth drawing:
# magmux parses 24-bit SGR regardless of what TERM claims, and the mirror has
# to carry all three channels rather than quantising to the palette.
printf '  \033[90m%-12s%b' 'truecolor' "$reset"
n=60
i=0
while [ "$i" -lt "$n" ]; do
  h=$((i * 360 / n))
  f=$(((h % 60) * 255 / 60))
  case $((h / 60)) in
    0) r=255; g=$f;         b=0 ;;
    1) r=$((255 - f)); g=255; b=0 ;;
    2) r=0; g=255; b=$f ;;
    3) r=0; g=$((255 - f)); b=255 ;;
    4) r=$f; g=0; b=255 ;;
    *) r=255; g=0; b=$((255 - f)) ;;
  esac
  printf '\033[48;2;%d;%d;%dm %b' "$r" "$g" "$b" "$reset"
  i=$((i + 1))
done
printf '\n'

# ── the attribute mask ──────────────────────────────────────────────────────
# UNDERLINE IS SGR 21 HERE, NOT SGR 4, AND THAT IS DELIBERATE.
#
# magmux suppresses SGR 4: `mux/vt.go:885` is `case p == 4:` with an empty body
# and the comment "suppress like MTM". SGR 21 (double underline) is the
# sequence that actually sets `AttrUnderline`, which is the bit the frame
# protocol carries as 1<<6 and which both painters draw. Using SGR 4 would
# produce a card with no underline on it, and the mirror would look like it had
# dropped an attribute when it had faithfully reproduced magmux's own screen.
# This cost real debugging time once; it should not cost it twice.
printf '  \033[90m%-12s%b' 'attributes' "$reset"
printf '\033[1mbold%b ' "$reset"
printf '\033[2mdim%b ' "$reset"
printf '\033[3mitalic%b ' "$reset"
printf '\033[21munderline%b ' "$reset"
printf '\033[7mreverse%b ' "$reset"
printf '\033[9mstrike%b ' "$reset"
printf '\033[53moverline%b ' "$reset"
printf '\033[5mblink%b ' "$reset"
printf '\033[38;5;208;48;5;17;1mfg+bg+bold%b' "$reset"
printf '\n'

rule
# The falsification instructions. A viewer who reads nothing else should still
# know how to prove the mirror is live rather than a screenshot of it, so the
# invitation names a command whose output is unmistakable and unpredictable.
printf '  \033[1mtry it:\033[0m type   \033[36mls --color=auto\033[0m   or   \033[36mdate +%%T\033[0m   or   \033[36mtop\033[0m   here and watch\n'
printf '          it appear in the pane below and in the browser. Type a typo and watch\n'
printf '          the cursor move. \033[90mNothing in this demo self-drives — every byte you see\n'
printf '          on the two clients was produced by this shell.%b\n\n' "$reset"

# An ORDINARY interactive shell for the rest of the run: the pane has to be
# typeable, which `demo/staged-agent.sh` (a script that never reads stdin)
# would not be.
exec "${SHELL:-/bin/sh}" -i
