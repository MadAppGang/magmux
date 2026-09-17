/**
 * ticket-runner — the demo magmux plugin.
 *
 * It exists to prove one claim end to end: a plugin can add ops that every
 * transport can call, open a session of its own, drive it, and report what it
 * sees — with no model, no network and no API key anywhere.
 *
 * What it does, per ticket:
 *
 *   1. `open_pane {controller:"self"}`, running fake-agent.sh. `self` is what
 *      makes THIS plugin the pane's controller, so its snapshots become the
 *      pane's state everywhere magmux reports state.
 *   2. Watch the pane's frames and wait for the agent's `> ` prompt.
 *   3. `send` the ticket.
 *   4. Push `working`, and emit `progress` as it goes.
 *   5. Keep watching for a `DONE:` line, and push `awaiting_input` carrying it.
 *
 * Step 5 is the point of the whole example. `awaiting_input` is what a driver
 * waits for, and it is a claim about a session magmux is not itself following:
 * the plugin is the observer, and magmux reconciles what it says with what the
 * terminal saw.
 *
 * Run:  magmux -e sh --plugin 'bun examples/plugins/ticket-runner/main.ts'
 * Call: {"type":"call","op":"ticket.run_ticket","args":{"title":"t1"},"id":1}
 */
import path from "node:path";
import { MagmuxPlugin, OpError, type Invocation, type PaneView } from "./sdk";

const AGENT = path.join(import.meta.dir, "fake-agent.sh");

/** How long a ticket's agent gets to print its prompt, and then to answer. */
const PROMPT_TIMEOUT_MS = 20_000;
const ANSWER_TIMEOUT_MS = 120_000;

type TicketState = "starting" | "sent" | "done" | "cancelled" | "failed";

interface Ticket {
  id: string;
  title: string;
  body: string;
  pane: number;
  state: TicketState;
  response: string;
  startedAt: number;
  endedAt?: number;
}

const tickets = new Map<string, Ticket>();
let seq = 0;

const plugin = new MagmuxPlugin({
  name: "ticket",
  version: "0.1.0",
  events: ["progress"],
  ops: [
    {
      name: "run_ticket",
      description:
        "Open a pane, run an agent in it, and give it a ticket to work on. Returns as soon as " +
        "the ticket has been sent; watch the `progress` events, or poll `status`, for the rest.",
      class: "control",
      schema: {
        type: "object",
        properties: {
          title: { type: "string", description: "One-line summary of the work." },
          body: { type: "string", description: "The detail, if any." },
          cmd: { type: "string", description: "Command to run instead of the bundled fake agent." },
        },
        required: ["title"],
      },
      handler: runTicket,
    },
    {
      name: "status",
      description: "Every ticket this plugin has run, with its pane, its state and its answer.",
      class: "read",
      schema: { type: "object", properties: { ticket: { type: "string" } } },
      handler: status,
    },
    {
      name: "cancel",
      description: "Stop following a ticket and close its pane.",
      class: "control",
      schema: {
        type: "object",
        properties: { ticket: { type: "string" } },
        required: ["ticket"],
      },
      handler: cancel,
    },
  ],
});

/**
 * Has the agent drawn its prompt?
 *
 * The trailing space is deliberately NOT part of the test. A frame's row text
 * is right-trimmed — trailing blanks are the rest of the screen, not content —
 * so the `> ` the agent printed arrives as `>`, and a plugin that waited for
 * the space it can see in its own terminal would wait forever.
 */
function hasPrompt(text: string): boolean {
  return text.split("\n").some((line) => line.trimEnd().endsWith(">"));
}

// ── ops ─────────────────────────────────────────────────────────────────────

async function runTicket(req: Invocation): Promise<Record<string, unknown>> {
  const title = String(req.args.title ?? "").trim();
  if (!title) throw new OpError("bad_request", "run_ticket needs a title");
  const body = String(req.args.body ?? "");
  const cmd = String(req.args.cmd ?? AGENT);

  const id = `t${++seq}`;
  const pane = await plugin.openPane({
    cmd,
    label: id,
    // The privileged field, and the only value it takes. It means "the plugin
    // on THIS connection", which magmux resolves from the registration rather
    // than from anything in the message — a plugin claims a pane by BEING one.
    controller: "self",
  });

  const ticket: Ticket = {
    id,
    title,
    body,
    pane,
    state: "starting",
    response: "",
    startedAt: Date.now(),
  };
  tickets.set(id, ticket);
  plugin.event("progress", pane, { ticket: id, step: "opened", cmd });

  // Claim the pane's state straight away. Until the first push a
  // plugin-observed pane reports `starting`, which is true but says nothing
  // about whose work it is.
  await plugin.pushSnapshot({
    pane,
    state: "working",
    project: "ticket",
    prompt: title,
  });

  const view = await plugin.watch(pane, 5);

  // Wait for the agent's prompt before typing. An agent that has not drawn its
  // prompt yet may not be reading its PTY, and an instruction delivered into
  // that window is simply lost — which is the same reason `send` pauses before
  // pressing Enter.
  const ready = await view.waitFor(hasPrompt, PROMPT_TIMEOUT_MS);
  if (!ready) {
    ticket.state = "failed";
    ticket.endedAt = Date.now();
    plugin.event("progress", pane, { ticket: id, step: "no_prompt" });
    await plugin.pushSnapshot({ pane, state: "error", error: "the agent never showed a prompt" });
    throw new OpError("timeout", `the agent in pane ${pane} never showed a prompt`);
  }

  const instruction = body ? `${title}\n${body}` : title;
  await plugin.send(pane, instruction, { label: id });
  ticket.state = "sent";
  plugin.event("progress", pane, { ticket: id, step: "sent", title });

  // Return NOW. The ticket is running; following it to the end is what the
  // events and `status` are for, and a caller that wanted to block would be
  // holding a request open for the length of somebody else's work.
  void follow(ticket, view);

  return { ticket: id, pane, state: ticket.state };
}

async function status(req: Invocation): Promise<Record<string, unknown>> {
  const want = String(req.args.ticket ?? "");
  const all = [...tickets.values()]
    .filter((t) => !want || t.id === want)
    .map((t) => ({
      ticket: t.id,
      pane: t.pane,
      title: t.title,
      state: t.state,
      response: t.response,
      elapsedMs: (t.endedAt ?? Date.now()) - t.startedAt,
    }));
  if (want && all.length === 0) throw new OpError("bad_request", `no ticket ${want}`);
  return { tickets: all, running: all.filter((t) => t.state === "sent").length };
}

async function cancel(req: Invocation): Promise<Record<string, unknown>> {
  const id = String(req.args.ticket ?? "");
  const ticket = tickets.get(id);
  if (!ticket) throw new OpError("bad_request", `no ticket ${id}`);
  if (ticket.state === "sent" || ticket.state === "starting") {
    ticket.state = "cancelled";
    ticket.endedAt = Date.now();
    await plugin.unwatch(ticket.pane);
    plugin.event("progress", ticket.pane, { ticket: id, step: "cancelled" });
    // The pane is left OPEN and reported as idle rather than closed: a
    // cancelled ticket's output is the most interesting thing about it, and a
    // caller that wants the space back has close_pane.
    await plugin.pushSnapshot({
      pane: ticket.pane,
      state: "awaiting_input",
      response: "cancelled",
    });
  }
  return { ticket: id, state: ticket.state };
}

// ── following a ticket ──────────────────────────────────────────────────────

/**
 * Watch one ticket's pane until its agent answers.
 *
 * The answer is the `DONE:` line, and finding it in the screen is the whole of
 * this plugin's "observation": a real one would parse its tool's own output
 * format, or read a file it writes. What matters is what happens next — the
 * push to `awaiting_input`, which is the state a driver waits for.
 */
async function follow(ticket: Ticket, view: PaneView) {
  const answered = await view.waitFor((text) => text.includes("DONE:"), ANSWER_TIMEOUT_MS);
  if (ticket.state === "cancelled") return;

  if (!answered) {
    ticket.state = "failed";
    ticket.endedAt = Date.now();
    plugin.event("progress", ticket.pane, { ticket: ticket.id, step: "timeout" });
    await plugin
      .pushSnapshot({ pane: ticket.pane, state: "error", error: "the agent never answered" })
      .catch(() => {});
    return;
  }

  const line = view
    .text()
    .split("\n")
    .map((l) => l.trim())
    .filter((l) => l.startsWith("DONE:"))
    .pop();
  ticket.response = line ?? "DONE:";
  ticket.state = "done";
  ticket.endedAt = Date.now();

  plugin.event("progress", ticket.pane, {
    ticket: ticket.id,
    step: "done",
    response: ticket.response,
  });
  // The claim a driver acts on: this session has finished its turn, and here is
  // what it said. magmux reconciles it with the terminal's own idle detection,
  // so the live `snapshot` and the shutdown `results` agree about this pane.
  await plugin
    .pushSnapshot({
      pane: ticket.pane,
      state: "awaiting_input",
      response: ticket.response,
      project: "ticket",
    })
    .catch(() => {});
  await plugin.unwatch(ticket.pane);
}

// ── main ────────────────────────────────────────────────────────────────────

await plugin.connect();
// Nothing else to do: magmux drives from here, and the connection closing is
// its teardown (results, shutdown, EOF). Exiting on it is what makes the
// SIGTERM magmux sends afterwards a formality.
await plugin.wait();
