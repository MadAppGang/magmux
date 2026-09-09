# magmux demo: multi-agent + controller, staged

A screenshot-ready magmux window: three agent panes being driven by one
controller, with the control panel open and the status bar carrying the same
digest. It costs nothing and needs no API key.

```bash
go build -o magmux .
demo/showcase.sh
```

Give it about 40 seconds to fill the panel, then take the screenshot whenever
you like — the controller keeps driving for roughly two minutes, so there is
always work in flight. `Ctrl-G q` quits.

| Variable | Default | Effect |
|---|---|---|
| `THEME` | `light` | `--theme` value; `dark` for the dark palette |
| `PANES` | `3` | Agent panes, 1 to 4 |
| `STAGGER` | `7` | Seconds between instructions |
| `SOCK_ID` | `demo` | Socket name, so two demos can run at once |

## What is real and what is staged

The agents are staged. `demo/staged-agent.sh` is not Claude Code — it prints
agent-shaped output on a script.

Everything else is the real thing, and that is the point of building it this
way rather than drawing a mock-up:

- The staged agent files a real Claude Code transcript under its own
  `$HOME/.claude/projects/<encoded-cwd>/staged.jsonl`, in the JSONL shape
  `controller_claude.go` parses. So magmux attaches a real
  `ClaudeCodeController` to each pane and reads model, prompt, response, tool
  and turn boundaries out of that file.
- `demo/staged-controller.sh` drives the panes over the documented socket only
  — `pilot`, `send`, `capture` — the same verbs `pilot/pilot.ts` and
  `magmux mcp` send. It holds one connection open for the whole run.
- The `◀ IN` rows are **not** sent by the controller and cannot be. They are
  written only by `recordObserved`, reached only from `pollControllers` — from
  what magmux itself saw a pane do. A controller can never fabricate a
  completion, which is the provenance rule the whole panel rests on.

`HOME` is repointed at `.task-grids/demo/home`, so the staged transcripts stay
fully isolated from your live Claude Code sessions. `task clean` removes them.

## What the frame contains

```
┌ reviewer ─────────────────┬ docs ──────────────────────┐
│ > re-read the patched hunk │ > note that a scrollback…  │
│   ⏺ Read  capture.go       │   ⏺ Edit  CLAUDE.md        │
├ tests ────────────────────┼ control panel ─────────────┤
│ > add a race run…          │ 15 sent │ 12 done │ 3 in…  │
│   ⏺ Bash  go test -race    │  0  ◐ WORKING  5/4   Bash  │
│                            │ 15:42 0 ▶ step 13/18 …     │
│                            │        ⇦ ok  52 bytes…     │
│                            │ 15:42 2 ◀ AWAITING   …     │
└────────────────────────────┴────────────────────────────┘
 * magmux │ 3/3 │ ▶15 ◀12 │ p0 working · Bash │ ctrl-g p hide
```

Four things worth pointing a reader at:

1. **Two directions, two sources.** `▶ OUT` is the controller's request as it
   arrived on the socket. `◀ IN` is magmux's own observation. The panel can
   show them disagreeing.
2. **The indented `⇦`** is magmux answering a request that carried an `id`. It
   renders on the row it answers and never closes a turn — an accepted request
   is not a completed turn.
3. **`sent / done / in flight`** is counted per route, not globally, so one
   pane going idle can never close another pane's outstanding step.
4. **The status bar** carries the same two numbers (`▶15 ◀12`) under the same
   provenance rule, because with the panel hidden it is the only place a
   controlled run announces itself.

## Running it against real agents instead

Two tasks drive live Claude Code sessions and cost real model budget:

```bash
task pilot:demo   # a pi.dev pilot steers one Claude Code session
task mcp:demo     # a Claude Code session splits its own magmux via MCP
```
