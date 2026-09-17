# ticket-runner — a magmux plugin, end to end

A plugin adds its own ops to magmux. They appear in `ops`, in `GET /v1/ops`, as
HTTP endpoints, over WebSocket, and as MCP tools — with no code in magmux that
knows what the plugin does.

`ticket-runner` is the smallest demo that proves the whole loop: it opens a pane
of its own, drives the session inside it, and reports what that session is doing
back to magmux. It needs **no model, no network and no API key** — the thing in
the pane is `fake-agent.sh`, a prompt-read-echo loop.

```
     run_ticket                    open_pane {controller:"self"}
client ────────▶ magmux ─invoke─▶ ticket-runner ────────────────▶ pane
       ◀────────        ◀─result─               ──send──────────▶ fake-agent.sh
         {ticket,pane}                          ◀─watch (frames)─
                       ◀─plugin.event progress──
                       ◀─controller.snapshot────  "awaiting_input, DONE: t1"
```

## Run it

```sh
magmux -e 'sh' --plugin 'bun examples/plugins/ticket-runner/main.ts'
```

Then, from anywhere that can reach the socket:

```sh
printf '{"type":"call","op":"ticket.run_ticket","args":{"title":"build the thing"},"id":1}\n' \
  | nc -U "$MAGMUX_SOCK"
```

or, with `--listen`:

```sh
curl -sS -X POST localhost:7777/v1/ops/ticket.run_ticket \
  -H "Authorization: Bearer $(cat /tmp/magmux-$PID.token)" \
  -H 'Content-Type: application/json' \
  -d '{"title":"build the thing"}'
```

Either way a new pane appears, the fake agent is given the ticket, and a second
or so later the pane settles at `awaiting_input` carrying `DONE: build the
thing`. `ticket.status` reports the same thing afterwards.

## The ops

| Op | Class | What it does |
|---|---|---|
| `ticket.run_ticket {title, body?, cmd?}` | control | Opens a pane, sends the ticket, returns `{ticket, pane}` **at once** |
| `ticket.status {ticket?}` | read | Every ticket, its pane, its state and its answer |
| `ticket.cancel {ticket}` | control | Stops following a ticket; leaves its pane open |

`run_ticket` returns as soon as the ticket has been sent, and the rest arrives
as `progress` events and as pane state. A call that blocked for the length of
someone else's work would hold an HTTP request open for minutes and tell a
caller nothing while it did.

## What each file is for

| File | |
|---|---|
| `main.ts` | The plugin: the three ops, and the loop that follows a ticket |
| `sdk.ts` | A minimal plugin SDK — connect, register, dispatch invokes, emit events, push snapshots, watch frames |
| `fake-agent.sh` | The thing in the pane. Prints `> `, reads a line, answers `DONE: <line>` |

The Go equivalent of `sdk.ts` ships in the `client` package
(`client.NewPlugin`), and is what a plugin written in Go would use.

## The three things worth copying

**1. `{"controller":"self"}` is how a plugin claims a pane.**

```ts
const pane = await plugin.openPane({ cmd, label: id, controller: "self" });
```

`self` means "the plugin on THIS connection", and magmux resolves it from the
registration rather than from anything in the message. Naming a plugin —
`"controller":"plugin:ticket"`, even correctly — is `bad_request`: a connection
claims a pane by *being* a registered plugin, never by naming one. The pane's
controller is then `plugin:ticket` everywhere magmux reports one.

**2. `controller.snapshot` is the claim a driver acts on.**

```ts
await plugin.pushSnapshot({ pane, state: "awaiting_input", response: "DONE: t1" });
```

magmux reconciles it with what the terminal itself saw, so the live `snapshot`
event and the shutdown `results` agree about the pane. Stop pushing — crash,
say — and the pane degrades to magmux's own observation rather than freezing on
the last thing the plugin said.

**3. Frames are whole rows, and row text is right-trimmed.**

Waiting for `"> "` never matches: the trailing space is blank screen, not
content. `main.ts` waits for a row that *ends with* `>`. This is the one thing
that catches everybody once.

## Writing your own

The environment magmux gives a `--plugin` process is the whole interface:

| | |
|---|---|
| `MAGMUX_SOCK` | The socket to dial |
| `MAGMUX_PLUGIN_TOKEN` | A one-time credential for `plugin.register` |
| `MAGMUX_PLUGIN_ID` | magmux's label for this process, before it has a name |

Its stdout and stderr go to `{sock-dir}/magmux-{id}.plugin-{name}.log` — never
to the terminal, which magmux is usually holding in raw mode with an alternate
screen on it. **That log is the first place to look when a plugin does not
appear in `ops`.**

The rules a plugin lives by:

- **Names.** Plugin `[a-z][a-z0-9-]{0,31}` (no underscore); op
  `[a-z][a-z0-9_]{0,29}`. The op is advertised as `<plugin>.<op>` and reaches
  MCP as `<plugin>__<op>`, which is why the plugin half has no underscore.
- **Declare your events.** A `plugin.event` naming an event that was not in
  `plugin.register` is refused. Events are ≤64 KB and ≤50/s; past the rate they
  are dropped and counted.
- **Classes are capability claims.** `read` is what a `--view-token` caller may
  reach for built-ins, and a plugin's own class never widens that on its own:
  an operator grants one plugin op to viewers with `--view-op plugin.op`.
- **Answer or be cancelled.** An invoke carries `deadlineMs`; when magmux stops
  waiting it sends `invoke_cancel` with the same `call` id. A plugin that
  ignores it is not killed — it just produces an answer nobody reads.
- **Dying is fine.** When the connection or the process ends, the ops are
  unregistered, `ops_changed` and `plugin_exited` are published, and calls in
  flight fail with `plugin_gone`. There is no restart in v1.
