#!/usr/bin/env bash
# The controller half of the staged demo: the thing that DRIVES the panes.
#
# It holds ONE socket connection open for the whole run, so magmux sees a
# controller attach and stay, exactly as the pi.dev pilot (pilot/pilot.ts) or
# `magmux mcp` does. Every line below is a documented socket verb — this script
# has no privileged access and cannot write a panel row directly:
#
#   pilot/start   fills the panel header (client, model, goal, planned steps)
#   send          injects text into a pane's PTY and files the ▶ OUT row
#   capture       a non-instruction request; with an id, magmux answers it and
#                 the answer renders as the indented ⇦ ack on that same row
#   pilot/note    a one-line aside in the panel
#   pilot/finish  closes the run and prints the summary
#
# The ◀ IN rows are NOT sent from here, and cannot be: recordObserved is
# reached only from pollControllers, i.e. from what magmux itself saw a pane
# do. That is the provenance rule the panel rests on — a controller can never
# fabricate a completion.
set -u
SOCK="${1:?usage: staged-controller.sh /tmp/magmux-<id>.sock}"
PANES="${PANES:-3}"
STEPS="${STEPS:-18}"
# Seconds between instructions. A driven turn takes 7-14s, so this stagger
# keeps roughly two panes mid-turn while the third rests — the mixed state a
# screenshot wants.
STAGGER="${STAGGER:-7}"

# Per-pane instruction pools. Each is plausible follow-on work for that agent,
# cycled so a long run never repeats the same line twice in a row.
r_work=(
  "re-read the patched hunk and confirm go vet is clean"
  "check whether captureAt needs the same bound"
  "look for a second unbounded row walk in rowsText"
  "confirm the p.mu order still matches the documented one"
)
t_work=(
  "add a race run and a benchmark to the proof"
  "re-run the whole suite against the patched capture path"
  "pin the allocation count with a fresh benchmark run"
  "run the scrollback tests under -count=5 for flakes"
)
d_work=(
  "document the len(row) bound in the scrollback invariants"
  "note that a scrollback row keeps its printed width"
  "record why nothing reflows on resize"
  "add the renderPane padding rule to the invariants"
)
p_work=(
  "list what is left before this can merge"
  "summarise the three findings in one paragraph"
  "check nothing else in the tree reads a row unbounded"
  "draft the commit message for the bound"
)

{
  s() { printf '%s\n' "$1"; sleep "$2"; }
  send() { # $1 pane, $2 label, $3 text
    printf '{"type":"send","pane":%d,"text":"%s","label":"%s"}\n' "$1" "$3" "$2"
    sleep "$STAGGER"
  }

  # Identity. No pane field, so this opens no route — it names the controller.
  s '{"type":"pilot","event":"start","client":"staged-controller/1.0","model":"anthropic/claude-sonnet-5","steps":'"$STEPS"',"goal":"Fix the scrollback capture bound, prove it, and document it"}' 3

  for i in $(seq 1 "$STEPS"); do
    pane=$(( (i - 1) % PANES ))
    case "$pane" in
      0) pool=("${r_work[@]}") ;;
      1) pool=("${t_work[@]}") ;;
      2) pool=("${d_work[@]}") ;;
      *) pool=("${p_work[@]}") ;;
    esac
    round=$(( (i - 1) / PANES ))
    send "$pane" "step $i/$STEPS" "${pool[$(( round % ${#pool[@]} ))]}"

    # Early in the run, show the two row kinds a `send` cannot produce: a
    # request magmux answers (the indented ⇦ ack) and a controller aside.
    if [ "$i" -eq "$PANES" ]; then
      s '{"type":"capture","pane":1,"lines":4,"id":41}' 2
      s '{"type":"pilot","event":"note","label":"patch verified","text":"docs landed; widening the proof"}' 2
    fi
  done

  s '{"type":"pilot","event":"finish","summary":"bound applied, 47 tests + race clean, invariants updated"}' 0

  # Hold the connection open so the panel keeps showing a controller attached,
  # but poll rather than sleep: during a long sleep this script writes nothing,
  # so it would never notice magmux exiting and would outlive it. An orphan
  # holding the same --id path would then inject into the NEXT demo run.
  while [ -S "$SOCK" ]; do sleep 2; done
} | nc -U "$SOCK"
