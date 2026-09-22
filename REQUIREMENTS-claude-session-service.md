# Requirements — Claude Code session-state service

**For:** the magmux developer
**From:** Jack (via a research session that measured everything below on the live machine)
**Status:** requirements, not a design. The shape is yours to choose.

---

## 1. The problem

Jack runs **30+ Claude Code sessions concurrently**, spread across ~14 tmux
sessions and ~33 windows. There is no way to see which of them need him.

A session blocked twenty minutes on a permission prompt looks exactly like one
busy working and one that finished an hour ago. He finds out by visiting panes
one at a time. With 33 windows that does not scale, and it is the single
biggest friction in his day.

**What he asked for, verbatim:**

> "we need to have some agent which deeply integrates with Claude Code session
> and in real time has session state, it knows where user input is required,
> how many subagents there are, if some error happened. So that agent monitors
> all Claude sessions across all tabs and surfaces that information as a
> service."

---

## 2. Read this first — a correction that shapes everything

**magmux cannot see any of these sessions today.**

magmux is a terminal multiplexer — a Go port of MTM, an *alternative* to tmux.
It never execs tmux. A repo-wide grep of non-test Go finds exactly one
`os.Getenv("TMUX")`, used only to word an advisory string, and the README states
it outright: *"magmux never execs `tmux` itself."*

Jack's Claude sessions all run inside **tmux**, not magmux. So:

- `ClaudeCodeController` cannot help as-is. It is bound to a `*Pane` magmux
  itself forked — construction takes a `*Pane`, discovery reads `p.cmd.Args`,
  and there is a deliberate heuristic to **reject** sessions it did not spawn
  (*"anything in this set belongs to some other session"*). This requirement is
  the exact inverse: observe sessions magmux did **not** start.
- Any sidebar rendered by magmux's `ControlPanel` is only visible to someone
  running magmux as their multiplexer. Jack is not. His UI must land in tmux.

This does not disqualify magmux as the **host** — it has the daemon, the op
registry, the socket/HTTP/WS/SSE transports and an MCP server, all of which this
service needs and none of which exist elsewhere. But the tmux-facing parts are
**net-new work**, not a hook into existing machinery. Please size accordingly,
and push back if you think a different home is right.

---

## 3. What already exists — do not rebuild these

Measured on the live machine. Each of these took real effort to establish.

### 3.1 Claude Code already publishes a live session registry

`~/.claude/sessions/<pid>.json`, one file per running session, **rewritten by
Claude itself**. This is the single most valuable input and magmux does not read
it today.

```json
{"pid":98063,"sessionId":"d1f05570-...","cwd":"/Users/jack/dotfiles",
 "startedAt":1789451498980,"version":"2.1.272",
 "kind":"interactive","entrypoint":"cli",
 "tmux":"magai:@191.%359",
 "messagingSocketPath":"/tmp/cc-socks/98063.sock",
 "name":"dotfiles-27","nameSource":"derived","nameSince":...,
 "status":"idle","statusUpdatedAt":...,"updatedAt":...,
 "formerNames":[{"name":"...","until":...}],
 "waitingFor":"permission prompt"}
```

Full key union: `cwd, entrypoint, formerNames, kind, messagingSocketPath, name,
nameSince, nameSource, peerFeatures, peerProtocol, pid, pidDomain, procStart,
sessionId, startedAt, status, statusUpdatedAt, tmux, updatedAt, version,
waitingFor`.

**It answers most of the brief directly:**

| Requirement | Field |
|---|---|
| "where user input is required" | `status: "waiting"` + `waitingFor: "input needed"` / `"permission prompt"` |
| which tmux pane | `tmux: "<session>:@<window>.%<pane>"` |
| session identity | `name`, `nameSource` (`user`/`auto`/`derived`), `formerNames` |
| liveness | `pid` |

Observed `status` values: `idle`, `busy`, `shell`, `waiting`.

**Three traps, all measured:**

1. **Records are never deleted.** 32 of 63 files belonged to dead pids. Liveness
   must be probed (`kill -0`), and for tmux-bound sessions the pane must still
   exist too — both gates, or dead sessions paint live windows.
2. **`statusUpdatedAt` goes stale.** One live session's was ~3 days old while its
   pid was alive. `status` must be cross-checked against transcript mtime, never
   trusted alone.
3. This is an **undocumented contract**. Treat it as a fast path with a fallback,
   not the only way in.

### 3.2 The colour and other per-session facts live in the transcript

`~/.claude/projects/<slug>/<sessionId>.jsonl`.

```json
{"type":"agent-color","agentColor":"purple","sessionId":"..."}
```

Last record wins. **The key is `agentColor`, not `color`** — three separate
searches missed this because they grepped `"color"`, and the only `"color"` keys
in a transcript belong to *subagents*, beside `agent_type`/`team_name`.

Validated by experiment: a fresh session has zero such records; `/color green`
writes one; `/color purple` supersedes it; **spawning a real subagent writes
none**. Machine-wide, 9 transcripts contain subagents and none has an
`agent-color` record, while 29 of the 31 sessions that ran `/color` do.

**Locating the transcript: glob for `<sessionId>.jsonl` under
`~/.claude/projects/*/`. Do not derive the directory from `cwd`.** A
cwd-derived slug measured **95.86%** across 356 real project dirs and cannot be
fixed — underscores are converted too, and worktree sessions sometimes collapse
to the parent repo's directory. The glob is self-validating and costs ~2.5 ms.

**Transcripts reach 625 MB.** A blind full scan is not viable. A 200 KB tail
agreed with a full scan on 30/30 transcripts tested, because the record is
rewritten on each append; keep a bounded fallback for a colour set early in a
very long session.

### 3.3 An unused push channel

Every session advertises `messagingSocketPath` — `/tmp/cc-socks/<pid>.sock`,
**162 live sockets** — with `peerFeatures: ["notify_idle", ...]`.

**Nothing on the machine consumes it.** Prior research described it as a
write-only text inbox; that was not verified against a source. If it can deliver
idle/input events, it is the only true real-time channel and would remove all
need for polling. **Worth investigating first** — it may collapse the hardest
requirement in this document.

### 3.4 What Claude does NOT give you

Measured three independent ways: **`/color` fires no event at all.** It does not
fire `UserPromptSubmit` (proved with a scoped hook probe — typing `/color red`
produced no invocation while a normal message immediately after did), it
produces no assistant turn so no `Stop` hook, and it causes no statusline
re-render. Built-in slash commands bypass the prompt pipeline entirely.

Assume the same of any other built-in command. State changes that matter may
leave **no event**, only a file change.

### 3.5 The statusline is a real-time wire — for identity, not colour

A `statusLine` command is invoked with JSON on stdin and **inherits `$TMUX_PANE`**
(verified: `TMUX_PANE=[%362]`). It is event-driven, not a timer — measured **0
renders in 20 s idle, 1 render per turn**.

Full payload schema at 2.1.272:

```
session_id, session_name, transcript_path, cwd, scratchpad_dir,
effort{level}, model{id,display_name},
workspace{current_dir,project_dir,added_dirs},
version, output_style{name},
cost{total_cost_usd,total_duration_ms,total_api_duration_ms,
     total_lines_added,total_lines_removed},
context_window{total_input_tokens,total_output_tokens,context_window_size,
               current_usage,used_percentage,remaining_percentage},
exceeds_200k_tokens, fast_mode, thinking{enabled}
```

`session_name` appears when the name is user-set. **No colour field.**

### 3.6 Already built and shipped — the tab surface

`tmux-claude-continuity` (github.com/MadAppGang/tmux-claude-continuity) now
paints tmux window tabs with each session's colour and name. It is hook-driven,
has no daemon, and ships 1006 test assertions. Reuse its rules rather than
re-deriving them:

- **Ownership:** the live Claude at the **lowest `pane_index`** in the window.
  Not index 0 — `pane-base-index` is 1 here and **none** of 33 live sessions sat
  at 0. A window can hold **three** Claude sessions.
- **Liveness:** `kill -0` **and** the pane exists.
- `cc_colour.sh` is the only place either repo names Claude's schema, enforced
  by a test. If Claude changes the format, one file moves.

Its reconciler CLI is directly consumable:
`cc_tab_reconcile.sh [--all | @<win> | %<pane>]`, and `CC_TAB_DEBUG=1` prints
the computed plan without writing.

### 3.7 Other existing pieces

- **`tmux-mcp`** (`/Users/jack/mag/tmux-mcp/`) — **13 instances running, one per
  Claude session.** tmux-native, long-lived, with a trigger engine and a push
  channel into Claude (`notifications/claude/channel`). Its event vocabulary is
  already close to what this service needs: `exit · error · user_input · shell ·
  idle:N · bell · pattern:<regex> · timeout`. Pane-scoped; knows nothing of
  `~/.claude/sessions/`.
- **`cc_popup.sh --list`** — an 18-column TSV tree inventory, documented as a
  contract, already includes Claude session ids.
- **`plugins/stats`** — a dormant SQLite store at `~/.claude/stats.db`
  (schema v1) with per-tool timing and activity classification.
- **`tools/tt`** — Jack's Go/bubbletea tmux session TUI. **Zero Claude
  awareness.** A clean insertion point if a tmux-side TUI is wanted.

---

## 4. Functional requirements

### R1 — Discovery: every Claude session on the machine

Enumerate **all** live Claude Code sessions, including those the service did not
spawn and those inside tmux. Sessions appear and disappear constantly; discovery
must be continuous, and stale records must never surface as live.

### R2 — State model

Per session, at minimum:

| Field | Source | Notes |
|---|---|---|
| identity | `sessionId`, `pid`, `name`, `nameSource` | `formerNames` exists too |
| location | `cwd`, `tmux` (session/window/pane) | may be a worktree |
| **status** | `idle` / `busy` / `shell` / `waiting` | must be staleness-corrected |
| **needs input** | `waitingFor` | `"input needed"` vs `"permission prompt"` — distinguish them, they need different responses |
| **blocked for how long** | derived | required for the delay-based alerting in R6 |
| **subagent count** | transcript / `subagents/` dir | currently discarded by every existing tool |
| **errors** | transcript | nothing surfaces these outside a pane today |
| colour | `agentColor` (transcript) | §3.2 |
| cost / context % | statusline payload | §3.5 |

Model `waiting` as first-class. It is the entire point of the feature.

### R3 — Real time, without a polling daemon

**This is a hard constraint from Jack, and his machine's history justifies it:**
226 idle MCP processes squatting 17.3 GB; 61 orphaned probe scripts busy-spinning
across 12.5 of 18 cores; hung mount daemons stalling the UI. Resident background
processes have repeatedly degraded this machine.

Requirements:

- Push where a push channel exists. **Investigate `/tmp/cc-socks/<pid>.sock` and
  `notify_idle` first** (§3.3) — it may remove the problem entirely.
- Where no event exists, prefer reacting to activity the user is already
  generating over a timer.
- Cost **nothing measurable when nothing is happening**. An idle machine with 30
  sessions should show no CPU attributable to this service.
- If a timer is unavoidable, it must be justified with a measurement and be
  configurable, including *off*.

For scale: reading one session's colour costs ~2.5 ms to locate plus ~18 ms to
resolve; a full 33-window tmux reconcile is 474–675 ms, and a single window ~65
ms. Whatever you build, the all-sessions path needs to stay well clear of the
per-window path in cost.

### R4 — Surfaces (all four are wanted)

1. **Queryable CLI / JSON** — the service layer. Everything else consumes it;
   scripts, status bars and other agents included. This is the primary
   deliverable; the rest are clients.
2. **tmux window tabs** — already shipped (§3.6). Ideally re-point it at this
   service rather than having two readers of Claude's schema.
3. **A side panel in tmux** — *"like Herd, a list of session agents and their
   states on the left."* Jack's words: **"a real tmux panel on the left or on
   the right, but toggled on and off."** A real pane, not a popup. It must
   render inside **tmux**.
4. **macOS notifications** — nothing on the machine has a macOS-native or
   menubar notification path today. Must be filtered (see R6): 30 sessions
   firing unfiltered alerts would be unusable.

### R5 — Actions (the service may act, not only report)

Jack explicitly chose a full agent over a read-only monitor. Required, in
increasing order of risk:

1. **Navigate / focus** — jump the client to the session needing attention.
   Safe; touches only the view.
2. **Answer permission prompts** — approve or deny by rule. Blocked sessions are
   the main pain point.
3. **Send input / nudge** — type into a waiting session.
4. **Lifecycle** — restart, kill, resume a wedged session.

Actions 2–4 act on live agents on Jack's behalf. They need an explicit
allowlist, an audit trail of what was done and why, and a way to see and undo
recent actions.

### R6 — Configurable policy

Jack's answer to both "how autonomous?" and "when should it notify?" was
**"configurable"**. One config file should govern both. Sketch, not a spec:

```toml
[notify]
enabled = true
on      = ["waiting", "error"]
delay   = "2m"          # only interrupt after this long blocked
quiet   = ["22:00-08:00"]

[actions]
navigate  = "always"    # always | ask | never
approve   = "rule"      # rule | ask | never
send      = "ask"
lifecycle = "ask"

[[actions.rules]]       # explicit allowlist only
match = { cwd = "~/mag/**", tool = "Read" }
do    = "approve"
```

Suggested default posture: **navigate freely, everything else asks.** Nothing
acts unattended without a rule the user wrote.

---

## 5. Non-functional requirements

- **Read-only toward Claude's state.** 30+ live Claude processes own
  `~/.claude/`. Never write there.
- **Never destabilise tmux.** A `run-shell` whose client has gone away
  segfaulted the tmux server and **killed all 16 sessions (2026-08-16)**. Every
  such call must carry `-b` and `>/dev/null 2>&1 || true`. Never invoke
  `tmux run-shell` from a tool client.
- **Test isolation.** Any tmux server started for tests needs `-f /dev/null` and
  a private socket. A scratch server that read `~/.tmux.conf` autoloaded
  resurrect/continuum and **cloned the entire workspace** — hundreds of
  processes, 43% of the machine. A 2026-08-17 run destroyed 16 sessions, 44
  windows and 71 panes.
- **Degrade silently.** Claude's on-disk formats are undocumented and will
  change. An unrecognised value must never produce a wrong answer — prefer "no
  opinion", and log the raw value so the new format is discoverable.
- **Distinguish "no opinion" from "no".** In the tab feature, `""` (no record)
  and `"default"` (explicitly uncoloured) are deliberately different: collapsing
  them would let a future Claude build silently erase every manual choice. The
  same distinction will matter here.

---

## 6. Acceptance criteria

1. Lists every live Claude session on the machine, and **excludes** the ~half of
   `~/.claude/sessions/*.json` records whose pids are dead.
2. Correctly reports the session(s) currently `waiting`, and distinguishes
   `"input needed"` from `"permission prompt"`.
3. Corrects a session whose `statusUpdatedAt` is days stale while its pid lives.
4. Reports subagent counts — no existing tool does.
5. Surfaces an error from a session without opening its pane.
6. Idle cost is measurably zero with 30 sessions open and nothing happening.
7. The tmux side panel toggles on and off and matches the CLI's JSON exactly.
8. A notification honours `delay` and `quiet` hours.
9. Every destructive action is exercised against a disposable session on an
   isolated tmux server, never the live one.
10. `tmux-claude-continuity`'s 1006 assertions still pass if the tab surface is
    re-pointed at this service.

---

## 7. Open questions for you

1. **Is magmux the right host at all?** Given §2, a tmux-native tool may be the
   honest answer, with magmux consuming the same service. Jack chose magmux
   before knowing it cannot see tmux. Say so if you disagree with the premise.
2. **Does `/tmp/cc-socks/<pid>.sock` deliver events?** If yes, most of R3
   dissolves. This is the highest-value unknown in the document.
3. **Is `tmux-mcp` a better home for the tmux-facing half?** It is already
   tmux-native, already per-session, and already has the event vocabulary —
   though it is already 13 resident processes, which cuts against R3.
4. **Subagent state:** is the `subagents/` directory sufficient, or does it need
   transcript parsing? Note teammate colours live in
   `subagents/agent-*.meta.json` under `color`, and only for
   `taskKind: in_process_teammate` — plain subagents have no colour at all.
5. **How should actions be audited?** R5 grants real power over live agents and
   Jack will want to see what was done on his behalf.
