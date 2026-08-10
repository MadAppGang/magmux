#!/usr/bin/env bun
/**
 * magmux pilot — an external AI agent that guides an interactive coding
 * session through a multi-step task.
 *
 * The pilot is a pi.dev agent (pi.dev, @earendil-works/pi-coding-agent) with
 * its normal toolbox removed and exactly two tools put in its place:
 *
 *   send_to_session(instruction)  type an instruction into the session,
 *                                 wait for the turn, return what happened
 *   finish(summary, success)      declare the task over
 *
 * That is the whole design. `send_to_session` blocks until magmux observes
 * the controlled pane settle, and hands the session's own answer back as the
 * tool result — so the multi-step loop (instruct → observe → decide) is just
 * pi's ordinary agent loop, with no orchestration logic layered on top.
 *
 * Run it as a magmux pane and it finds its multiplexer through MAGMUX_SOCK:
 *
 *   magmux -c -w \
 *     -e 'claude' \
 *     -e 'bun pilot/pilot.ts --pane 0 --goal "get the test suite green"'
 */
import {
  createAgentSession,
  defineTool,
  ModelRuntime,
  SessionManager,
} from "@earendil-works/pi-coding-agent";
import { Type } from "typebox";
import { MagmuxBridge, type TurnResult } from "./magmux.ts";

// ── output ────────────────────────────────────────────────────────────────
// The pilot usually runs in its own pane, so its stdout is part of the UI.
// Same semantic palette as the control panel: blue = pilot, green = session.

const C = {
  reset: "\x1b[0m",
  dim: "\x1b[2m",
  bold: "\x1b[1m",
  out: "\x1b[38;2;52;152;219m",
  in: "\x1b[38;2;46;204;113m",
  warn: "\x1b[38;2;255;180;84m",
  err: "\x1b[38;2;255;107;107m",
  grey: "\x1b[38;2;108;112;134m",
  text: "\x1b[38;2;205;214;244m",
};

const stamp = () => new Date().toTimeString().slice(0, 8);
const log = (color: string, glyph: string, head: string, body = "") => {
  process.stdout.write(
    `${C.grey}${stamp()}${C.reset} ${color}${glyph} ${head}${C.reset}\n` +
      (body ? `  ${C.text}${body}${C.reset}\n` : ""),
  );
};

// ── args ──────────────────────────────────────────────────────────────────

interface Args {
  pane: number;
  goal: string;
  steps: number;
  maxSteps: number;
  model?: string;
  sock: string;
}

function parseArgs(argv: string[]): Args {
  const get = (flag: string) => {
    const i = argv.indexOf(flag);
    return i >= 0 && i + 1 < argv.length ? argv[i + 1] : undefined;
  };
  const goal = get("--goal");
  if (!goal) {
    console.error("pilot: --goal is required");
    process.exit(2);
  }
  const sock = get("--sock") ?? process.env.MAGMUX_SOCK ?? "";
  if (!sock) {
    console.error("pilot: no socket — pass --sock or run inside magmux (MAGMUX_SOCK)");
    process.exit(2);
  }
  return {
    pane: Number(get("--pane") ?? 0),
    goal,
    steps: Number(get("--steps") ?? 0),
    // A hard ceiling on instructions. The pilot is spending real model time on
    // both sides, so a confused agent must not be able to loop indefinitely.
    maxSteps: Number(get("--max-steps") ?? 12),
    model: get("--model"),
    sock,
  };
}

// ── system prompt ─────────────────────────────────────────────────────────

const SYSTEM_PROMPT = `You are a pilot directing another AI coding agent through a task.

WHAT YOU ARE DIRECTING

The session on the other end is a full coding agent, not a shell and not a
junior. It reads and writes files, runs commands, searches the codebase, checks
its own work, and chooses its own tools. It is often a stronger model than you
are. Your job is to hold the objective and judge the results — not to decide
how the work gets done.

Delegate outcomes, not keystrokes. You are a tech lead giving direction to a
capable engineer, not a script driving a terminal.

  BAD   run: echo "alpha" > relay.txt && cat relay.txt
  GOOD  Create relay.txt with alpha as its first line.

  BAD   run: cat relay.txt
  GOOD  Confirm what relay.txt contains now and tell me in words.

  BAD   run: go test ./... 2>&1 | tail -20
  GOOD  Run the test suite and tell me which tests fail and why.

Never send a shell command as an instruction. If you catch yourself writing
one, you are doing the session's job for it — state the outcome you want
instead and let it choose the command. Do not name the tool to use, do not
prescribe flags, do not dictate the file-editing method.

THE PLAN IS YOURS TO MAKE

You receive ONE task. You do not receive a plan, and you must not wait to be
given one. Before your first instruction, work out for yourself what the
milestones are: what has to be true first, what depends on what, where you will
want to see evidence before continuing.

Nobody tells you how many steps there are. If the task implies an obvious
sequence, follow it. If it does not, decide one — and revise it as results come
back. A milestone you invented and then abandoned because the first result
surprised you is the system working, not a mistake.

ALTITUDE — the part that is easy to get wrong in both directions

An instruction is ONE OBJECTIVE. Not one command, and not the whole goal.

  TOO LOW    run: echo "alpha" > relay.txt && cat relay.txt
             (you are typing for it)

  TOO HIGH   <the entire goal, forwarded verbatim in one instruction>
             (you have delegated your own job and learn nothing until the end)

  RIGHT      Create relay.txt with alpha as its only line, then tell me what
             the file contains.

Handing the whole goal over in a single instruction is not delegation, it is
abdication. You exist to hold the objective across turns: to see a result, judge
it, and decide what comes next. If you send everything at once you cannot catch a
wrong turn, and there was no reason to have a pilot at all.

So: break the goal into a few meaningful milestones and work through them, one
per instruction, checking the result of each before the next. A milestone is a
unit you would give a person before checking in — usually two to five for a
small task.

If the goal constrains HOW the work should proceed ("incrementally", "one
module at a time", "verify after each change"), that is a constraint on YOUR
decomposition. Execute it yourself across several instructions; do not paste it
into a single instruction and let the session do it internally.

YOUR TOOLS

You cannot act on your own. Calling a tool is the only way anything happens:

- send_to_session: give the session one instruction and wait for it to finish
  that turn. You get back what it did.
- finish: end the run, with a summary and whether the goal was met.

Describing what you intend to do accomplishes nothing — the session never sees
your reply, only the text you pass to send_to_session.

HOW TO WORK

- One objective per instruction, and never the whole goal in one. An objective
  is a meaningful unit of work ("add the parser and make it compile"), not a
  single command. Do not split finer than you would when delegating to a
  person, and do not lump the entire task into one hand-off.
- Read the result before deciding the next instruction. The result is the
  session's own report, not proof. If it is vague ("done", "fixed"), ask it to
  state the concrete outcome — what the file now contains, what the tests
  printed — in its own words.
- If a turn reports no text, that is normal for a turn that was all tool calls.
  It does not mean failure. Ask for the outcome in words as part of your NEXT
  instruction rather than spending a turn re-checking.
- If a result contradicts what you expected, investigate before proceeding.
  Never send the next step as though the previous one had succeeded.
- Trust the session's judgement on approach. Correct it on outcomes, not style.
- If it stalls, or blocks on a permission you cannot grant, call finish with
  success=false rather than retrying blindly.
- Call finish as soon as the goal is met. Do not pad the run with verification
  the results already cover.

Every reply you make must contain a tool call. There is nothing else to do.`;

// ── main ──────────────────────────────────────────────────────────────────

async function main() {
  const args = parseArgs(process.argv.slice(2));

  const bridge = new MagmuxBridge(args.sock, { pane: args.pane });
  await bridge.connect();

  log(C.out, "◆", `pilot connected to pane ${args.pane}`, args.goal);

  // Pick the model before announcing, so the panel can show which model is
  // doing the driving.
  const modelRuntime = await ModelRuntime.create();
  let model;
  if (args.model) {
    const [provider, ...rest] = args.model.split("/");
    model = modelRuntime.getModel(provider, rest.join("/"));
    if (!model) {
      console.error(`pilot: model ${args.model} not found or has no credentials`);
      process.exit(2);
    }
  } else {
    const available = await modelRuntime.getAvailable();
    if (available.length === 0) {
      console.error("pilot: no models available — configure a provider for pi first");
      process.exit(2);
    }
    model = available[0];
  }
  const modelName = `${model.provider}/${model.id}`;

  bridge.announce(args.goal, args.steps, modelName);
  log(C.grey, "·", `pilot model ${modelName}`);

  // Wait for the session to be ready for its first instruction. Sending into
  // a still-booting session would land the text in a TUI that has not drawn
  // its input box yet, and be silently lost.
  log(C.grey, "·", "waiting for the session to come up…");
  if (!(await bridge.waitUntilReady())) {
    bridge.finish("session never became ready", true);
    log(C.err, "✗", "session never reached awaiting_input");
    bridge.close();
    process.exit(1);
  }
  log(C.in, "◀", "session ready");

  let sent = 0;
  let limitHit = false;
  let done: { summary: string; success: boolean } | null = null;

  const sendTool = defineTool({
    name: "send_to_session",
    label: "Send to session",
    description:
      "Give the controlled AI coding session one instruction and wait for it to " +
      "finish that turn. Returns what the session did, including whether it ended " +
      "waiting for input, blocked on a permission prompt, errored, or stalled.\n\n" +
      "The session is a full coding agent that picks its own tools and commands. " +
      "Instructions must be OUTCOMES in plain language, never shell commands — " +
      "say \"confirm what relay.txt contains and tell me in words\", not " +
      "\"run: cat relay.txt\". One objective per call; read the result before " +
      "deciding the next.",
    parameters: Type.Object({
      instruction: Type.String({
        description:
          "A high-level instruction stating the outcome you want, in plain " +
          "language, as you would brief a capable engineer. Not a shell command.",
      }),
      label: Type.Optional(
        Type.String({
          description: 'Short tag for the control panel, e.g. "step 2/5".',
        }),
      ),
    }),
    async execute(_id, params) {
      if (sent >= args.maxSteps) {
        limitHit = true;
        // Reported as a tool error rather than thrown, so the agent can react
        // to it (by calling finish) instead of the run dying mid-flight.
        return {
          content: [
            {
              type: "text" as const,
              text:
                `Step limit reached (${args.maxSteps} instructions). No further ` +
                `instructions can be sent. Call finish now with what was achieved.`,
            },
          ],
          details: { limitReached: true },
          isError: true,
        };
      }
      sent++;
      const label = params.label ?? (args.steps ? `step ${sent}/${args.steps}` : `step ${sent}`);
      log(C.out, "▶", label, params.instruction);

      const result = await bridge.runInstruction(params.instruction, label);
      renderTurn(result);

      return {
        content: [{ type: "text" as const, text: describeTurn(result) }],
        details: result,
      };
    },
  });

  const finishTool = defineTool({
    name: "finish",
    label: "Finish",
    description:
      "End the run. Call this as soon as the goal is met, or when it cannot be " +
      "met and further instructions would not help.",
    parameters: Type.Object({
      summary: Type.String({
        description: "What was achieved, in one or two sentences.",
      }),
      success: Type.Boolean({
        description: "True if the goal was met, false otherwise.",
      }),
    }),
    async execute(_id, params) {
      done = { summary: params.summary, success: params.success };
      return {
        content: [{ type: "text" as const, text: "Run ended." }],
        details: done,
        // Stops pi's agent loop after this tool batch, so the pilot does not
        // keep reasoning after it has declared the task over.
        terminate: true,
      };
    },
  });

  const { session } = await createAgentSession({
    model,
    // The pilot must not touch the filesystem itself — everything it wants
    // done goes through the session it is driving. Removing the built-ins is
    // what keeps that boundary real rather than a request in the prompt.
    //
    // "builtin", NOT "all": "all" starts with no tools enabled at all, which
    // silently strips the custom tools too. A pilot with zero tools does not
    // error — it just narrates what it would have done, and the run ends
    // looking clean while nothing happened.
    noTools: "builtin",
    customTools: [sendTool, finishTool],
    sessionManager: SessionManager.inMemory(),
  });
  session.agent.state.systemPrompt = SYSTEM_PROMPT;

  // Assert the tools survived. This is the failure that cost the most time to
  // diagnose, because its symptom is a model that "just won't follow
  // instructions" rather than anything resembling an error.
  const toolNames = session.agent.state.tools.map((t) => t.name);
  for (const required of ["send_to_session", "finish"]) {
    if (!toolNames.includes(required)) {
      console.error(
        `${C.err}pilot: tool ${required} is not registered — the agent has ` +
          `[${toolNames.join(", ") || "no tools"}] and cannot drive anything.${C.reset}`,
      );
      session.dispose();
      bridge.finish(`tool ${required} missing`, true);
      bridge.close();
      process.exit(2);
    }
  }
  log(C.grey, "·", `tools: ${toolNames.join(", ")}`);

  session.subscribe((event) => {
    if (event.type === "message_update" && event.assistantMessageEvent.type === "text_delta") {
      process.stdout.write(C.grey + event.assistantMessageEvent.delta + C.reset);
    }
  });

  // A weaker model will sometimes narrate its plan instead of calling a tool,
  // and pi's loop ends cleanly when a turn produces no tool calls — leaving a
  // run that "succeeded" without doing anything. Nudging once or twice is far
  // cheaper than failing the run, and if it still will not act, that is a
  // genuine result worth reporting rather than papering over.
  const NUDGE_LIMIT = 2;
  try {
    // --steps was previously only a label/meter input and never reached the
    // model, so the operator had no way to say how finely they wanted the
    // work broken up. It is a hint, not a quota: the pilot may need fewer or
    // more, and the max-steps ceiling is the real limit.
    // Only mentioned when the operator explicitly asked for a shape. Left
    // unset by default on purpose: naming a count is handing the pilot its
    // plan, and then the run only looks autonomous.
    const plan = args.steps > 0
      ? `The operator suggests roughly ${args.steps} milestones — treat that as a ` +
        `hint, not a quota.\n\n`
      : "";
    let prompt =
      `Task: ${args.goal}\n\n` +
      plan +
      `That is the whole brief — there is no plan attached, and no step count. ` +
      `Decide the milestones yourself, then direct the session through them one ` +
      `at a time, reading each result before choosing the next. Brief it the way ` +
      `you would brief an engineer: say what you want to be true, not what to type.\n\n` +
      `Start now by calling send_to_session — do not describe what you are going ` +
      `to do.`;

    for (let attempt = 0; ; attempt++) {
      await session.prompt(prompt);
      if (done) break;
      if (attempt >= NUDGE_LIMIT) break;
      log(C.warn, "!", "pilot replied without calling a tool — nudging");
      prompt =
        `You did not call a tool, so nothing happened. Your reply is not visible ` +
        `to the session. Call send_to_session now with the next instruction, or ` +
        `call finish if the goal is already met.`;
    }
  } finally {
    session.dispose();
  }

  let summary = done ?? {
    summary: "the pilot stopped without calling finish",
    success: false,
  };
  // An agent that ran out of steps wanted to keep going, so whatever it says
  // in its summary, the goal was not reached on its own terms. Observed in
  // practice: a pilot that exhausted its budget after two of three steps
  // still reported "successfully created all three lines". The pilot cannot
  // check the work, but it can refuse to launder a claim it has direct
  // evidence against.
  if (limitHit && summary.success) {
    summary = {
      success: false,
      summary: `step limit (${args.maxSteps}) reached before the goal was met — ` +
        `the pilot reported "${summary.summary}", which is not corroborated`,
    };
  }
  bridge.finish(summary.summary, !summary.success);
  log(
    summary.success ? C.in : C.err,
    summary.success ? "✓" : "✗",
    summary.success ? "finished" : "failed",
    summary.summary,
  );

  // Give the socket a moment to flush the finish before the process exits and
  // takes the connection with it.
  await new Promise((r) => setTimeout(r, 200));
  bridge.close();
  process.exit(summary.success ? 0 : 1);
}

/** Turn a TurnResult into the text the pilot model reads. */
function describeTurn(r: TurnResult): string {
  const secs = (r.durationMs / 1000).toFixed(0);
  switch (r.state) {
    case "awaiting_input":
      if (!r.response) {
        // A turn that only ran tools often leaves the controller with no
        // assistant text to report. Saying "(no response)" reads as "nothing
        // happened", and a pilot then burns its budget on sanity checks —
        // observed costing half a run. Say what it actually means and what to
        // do about it.
        return (
          `The session finished the turn in ${secs}s and is waiting for the next ` +
          `instruction.\n\nIt produced no text summary — normal when a turn is ` +
          `just tool calls${r.tool ? ` (last tool: ${r.tool})` : ""}. This does NOT ` +
          `mean the instruction failed, and the session is working normally. If you ` +
          `need to know the outcome, make it part of the next instruction — ask the ` +
          `session to state the result in its reply, in words.`
        );
      }
      return (
        `The session finished the turn in ${secs}s and is waiting for the next ` +
        `instruction.\n\nIt reported:\n${r.response}` +
        (r.tool ? `\n\nLast tool used: ${r.tool}` : "")
      );
    case "awaiting_permission":
      return (
        `After ${secs}s the session is BLOCKED on a permission prompt and cannot ` +
        `continue on its own. Last output:\n${r.response || "(none)"}`
      );
    case "error":
      return `The session reported an error after ${secs}s:\n${r.response || "(no detail)"}`;
    case "gone":
      return `The session process exited after ${secs}s. No further instructions can be sent.`;
    default:
      return (
        `The instruction did not produce a turn within ${secs}s — the session ` +
        `never started working. It may not have received the instruction. Do not ` +
        `assume the step was done.`
      );
  }
}

function renderTurn(r: TurnResult) {
  const secs = `${(r.durationMs / 1000).toFixed(0)}s`;
  switch (r.state) {
    case "awaiting_input":
      log(C.in, "◀", `awaiting_input · ${secs}${r.tool ? ` · ${r.tool}` : ""}`, r.response);
      break;
    case "awaiting_permission":
      log(C.warn, "◀", `blocked on permission · ${secs}`, r.response);
      break;
    case "error":
      log(C.err, "◀", `error · ${secs}`, r.response);
      break;
    case "gone":
      log(C.err, "◀", `session exited · ${secs}`);
      break;
    default:
      log(C.warn, "◀", `stalled · ${secs}`, "no turn started");
  }
}

main().catch((e) => {
  console.error(`${C.err}pilot: ${e?.stack ?? e}${C.reset}`);
  process.exit(1);
});
