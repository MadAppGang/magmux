# magmux agents demo: focus and notifications, by hand

`magmux agents` answers "which session needs me" by reading. Two of its parts
cannot be checked by reading anything back:

- **`focus`** (R5.1) moves the keyboard focus of a running magmux.
- **the notifier** (R4.4) interrupts you with a macOS banner.

Both need a session in a known state at a known time, which no live session
will do on request. This demo provides one.

```bash
go build -o magmux ./cmd/magmux
task agents:demo
```

## What is real and what is staged

The **session** is staged: one registry record `demo/agents.sh` writes and
rewrites, carrying the pid of its own pane process. That is all.

Everything else is the shipped code, reached exactly as a live session reaches
it: the registry source and its fsnotify watch, the per-session actor and its
middleware chain, the notify stage and its one-shot alarm, the audit writer,
the socket pane table, the ancestry walk, and the actuation gate.

It stages its own registry under `/tmp/magmux-agents-demo` and points magmux at
it with `CLAUDE_CONFIG_DIR`. Two consequences, both deliberate:

- The 40+ live Claude Code sessions on this machine are invisible to it, so no
  unrelated session can raise a banner at you. A notification test against the
  live registry announces other sessions and proves nothing about the one you
  meant to watch.
- Nothing under `~/.claude` is read, written or listed. `XDG_CONFIG_HOME` is
  redirected, so your own `~/.config/magmux/sessions.json` is not read, and
  `XDG_STATE_HOME` is redirected, so your real audit trail is not appended to.

The sandbox lives in `/tmp` rather than in the repo for two reasons that are
both the code's: an AF_UNIX socket path must be short, and the audit writer
refuses any path containing a `.claude` element, which a worktree checkout is
under.

## The panes

| Pane | What it is |
|---|---|
| `session` | the staged session. Its record holds this pane process's own pid. |
| `console` | a shell at the repo root, for the `task` commands below. |
| `audit` | the demo's audit trail, printed as it is written. |

```sh
task agents:status -- waiting     # the session now needs a human
task agents:status -- idle        # nothing outstanding
task agents:status -- busy        # it is working
task agents:errors -- on          # stage a transcript with one API error
task agents:focus                 # move focus to the session pane
task agents:table                 # the same document as a table
task agents:audit                 # the trail, once
```

`Ctrl-G a` opens the attention view, `Ctrl-G q` quits, then `task agents:down`
removes the sandbox.

## The checks, and what each one proves

| # | Do this | It must |
|---|---|---|
| 1 | `task agents:status -- waiting` | turn the panel row `YES` at once, with the reason underneath |
| 2 | wait `notify.delay` (15s by default) | raise exactly one banner, titled `magmux-demo` |
| 3 | wait another minute | raise nothing more: one banner per episode |
| 4 | `status -- idle`, then `status -- waiting` | raise exactly one new banner: a new episode |
| 5 | focus the console pane, then `task agents:focus` | move the highlighted border to the session pane |
| 6 | `task agents:errors -- on`, then `status -- idle` | show `ERR 1`, `ATTENTION YES`, and raise an error banner |
| 7 | `task agents:demo -- --quiet 22:00-08:00` inside those hours | announce nothing, and record one `refused` line naming the window |

Check 7 is the one that proves silence is recorded rather than silent. To run it
at any hour, pass a window that covers now.

## What the focus action does

`focus` changes which pane of a running magmux has the keyboard. It sends no
input, signals no process, and writes nothing the session owns.

The work is in finding the pane, because the command is not a magmux and has no
pane table of its own:

1. **Name the session.** The target is a pid, a session id, an unambiguous
   prefix of one, or a name. Two matches are refused with both listed.
2. **Ask every magmux for its panes**, over the socket. The connect-time
   aggregate already carries each pane's id and its child's pid, so no new verb
   was added.
3. **Walk the ancestry.** A session's pid is the tool's and a pane's pid is the
   shell's, so they are never equal and the walk is required. Up to 16 hops.
4. **Bind to one magmux** before using a pane id, because every magmux has a
   pane 0.
5. **Go through the gate.** Resolve the verb in the closed table magmux owns
   (`focus -> navigate`), check the policy, write the attempt line, act, write
   the outcome line.

The evidence is in the magmux window, not in the command's output: the
highlighted border moves. Focus the console pane first, or there is nothing to
see.

Two notes on that output:

- **`from magmux pane (none)` is not a claim that nothing was focused.** The
  socket's `focus` verb does not report which pane held focus before, so this
  path has no previous pane to name. Run inside magmux, the same actuator names
  the real one. The action is correct; the sentence is misleading.
- **`seq` counts per writer, not per file.** The magmux and each `agents focus`
  both number from 1 in one trail, so a line's identity is `(pid, seq)` and an
  outcome's `ref` resolves within its own pid. The audit renderer prints
  `<pid>#<seq>` for that reason.

The other three action tiers — `approve`, `send`, `lifecycle` — exist in the
schema and are refused in code by `Tier.Deferred`, before any policy is read.
A config saying `lifecycle = "always"` does not enable them.

## What the notifier does

It is a per-session middleware in the pipeline, not a sink and not a timer
sweep:

```
a registry record changes
  └─ fsnotify wakes the source (no polling)
       └─ that session's actor goroutine runs its chain
            ├─ pane gate    which pane, if any
            ├─ claudecode   subagents, API errors, colour
            ├─ notify       DECIDES whether a human should be told
            └─ verdict      the attention answer, always last
                 └─ coordinator: audit attempt → osascript → audit outcome
```

The stage does four things and performs no I/O:

1. **Trigger.** `waiting` comes from the record. `error` comes from transcript
   records flagged `isApiErrorMessage`, never from a tool result's own
   `is_error`, which marks errors the session recovered from. `waiting` wins
   when both hold, because a human can act on it in the next thirty seconds. A
   session whose process is determined dead, or whose pane is determined
   closed, never fires: about half the records on a real machine are stale, and
   a banner about a session nobody can help teaches you to ignore the banner
   that matters.
2. **Delay.** When a session qualifies but has not waited long enough, the
   stage returns the remainder through the pipeline's `Waker` interface, and its
   actor arms one one-shot alarm for exactly that. Nothing polls, and a session
   that will never qualify arms nothing.
3. **Episode latch.** One notification per episode, where an episode is
   identified by `statusSince`, the instant the session entered its current
   status. Waiting for an hour is one banner; waiting, being answered, working
   and waiting again is two.
4. **Quiet.** A quiet window suppresses; it does not defer. Holding a night of
   notifications until 08:00 delivers the storm the feature exists to prevent,
   moved to breakfast. The suppression is recorded with the window that caused
   it.

Delivery is separate from the decision, the same split the action gate makes:
audit attempt, `osascript`, audit outcome. A notification that cannot be
recorded is not sent, and the episode stays latched when that happens, so one
unwritable state directory cannot become a storm of banners about the failure
to raise banners.

A notification in the trail looks deliberately unlike an action:

```
28355#1  attempt  notify  tier=-  by=-  magmux-demo
      rule: …/sessions.json: notify.on contains "waiting", and it has been 5s (notify.delay = 5s)
28355#2  outcome  notify  tier=-  by=-  magmux-demo
      done: notified: magmux-demo — waiting for input for 5s
```

`tier` is null because a notification belongs to no action tier: it changes
nothing on any session. `requestedBy` is null because nobody asked; the policy
decided. Neither null is reachable on an action line that anything outside
magmux's own process can cause, so the two kinds of record are distinguishable
without knowing how either was made.

One in-process path collides, and it is a known limitation rather than a thing
you can trigger from here: a caller inside magmux that passes the reserved verb
`notify` with no surface and no tier is refused, and its refusal wears this same
shape. `CLAUDE.md` states it in full, and
`TestAReservedVerbWithNoSurfaceWearsANotificationsShape` holds it.

## Results that are not bugs

| What you see | Why |
|---|---|
| `PANE` is `-` in `task agents:table` | the CLI is not a magmux and has no pane table. The panel inside magmux fills it in. |
| `TIER` is always `authoritative` there | the inferred tier is built from a pane's traffic, and that process has no panes. |
| `SUB` and `ERR` are `-` before `errors -- on` | no transcript exists, so nobody counted. `-` is not `0`. |
| no banner at all | check the trail first: the attempt line is written before delivery, so a recorded attempt with no banner is a macOS notification-permission problem, and no line at all means the delay has not elapsed. |
