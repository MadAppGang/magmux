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

// The palette follows the background magmux resolved, which it hands every
// child as MAGMUX_THEME=light|dark.
//
// It used to be truecolor picked for a dark background, and `text` was
// rgb(205,214,244) — a near-white lavender. Every body line in the pane (the
// goal, and every instruction the pilot sends) is drawn with `text`, so on a
// light terminal the most important content on the screen was the least
// legible thing on it.
//
// A TUI would not have had this problem: it asks the terminal directly with an
// OSC 11 query, and magmux answers from the same resolution. The pilot is a
// plain script writing ANSI to a pipe, so it cannot ask — hence the variable.
//
// UNSET is a real case, not an error: the pilot runs outside magmux too, with
// --sock pointed at one. Then it names no colour it cannot justify — the
// default foreground (39) and dim (2), which are readable on whatever
// background the terminal actually has, and the basic ANSI accents (31-34),
// which every terminal theme maps to a shade legible against its own
// background.
type Palette = Record<
  "reset" | "dim" | "bold" | "out" | "in" | "warn" | "err" | "grey" | "text" |
  "ink" | "bgOut" | "bgIn" | "bgWarn" | "bgErr" | "bgMuted",
  string
>;

const BASE = { reset: "\x1b[0m", dim: "\x1b[2m", bold: "\x1b[1m" };

const PALETTES: Record<string, Palette> = {
  dark: {
    ...BASE,
    out: "\x1b[38;2;52;152;219m",
    in: "\x1b[38;2;46;204;113m",
    warn: "\x1b[38;2;255;180;84m",
    err: "\x1b[38;2;255;107;107m",
    grey: "\x1b[38;2;108;112;134m",
    text: "\x1b[38;2;205;214;244m",
    // Badge ink is the DARK end on a dark theme: the chip is a saturated
    // block, so the text on it has to contrast with the chip, not with the
    // page. Getting this backwards is how a badge turns into a smudge.
    ink: "\x1b[38;2;24;24;37m",
    bgOut: "\x1b[48;2;52;152;219m",
    bgIn: "\x1b[48;2;46;204;113m",
    bgWarn: "\x1b[48;2;255;180;84m",
    bgErr: "\x1b[48;2;255;107;107m",
    bgMuted: "\x1b[48;2;108;112;134m",
  },
  // Same hues, taken down to shades that hold contrast on a light background
  // rather than washing out against it.
  light: {
    ...BASE,
    out: "\x1b[38;2;21;101;177m",
    in: "\x1b[38;2;22;120;62m",
    warn: "\x1b[38;2;166;90;12m",
    err: "\x1b[38;2;178;38;38m",
    grey: "\x1b[38;2;108;112;134m",
    text: "\x1b[38;2;41;44;59m",
    ink: "\x1b[38;2;250;250;250m",
    bgOut: "\x1b[48;2;21;101;177m",
    bgIn: "\x1b[48;2;22;120;62m",
    bgWarn: "\x1b[48;2;166;90;12m",
    bgErr: "\x1b[48;2;178;38;38m",
    bgMuted: "\x1b[48;2;120;124;140m",
  },
  unknown: {
    ...BASE,
    out: "\x1b[34m",
    in: "\x1b[32m",
    warn: "\x1b[33m",
    err: "\x1b[31m",
    grey: "\x1b[2m",
    text: "\x1b[39m",
    // Basic ANSI BACKGROUNDS (44, 42, 43, 41) with bright-white ink. The
    // terminal maps each to a shade of its own theme, and white on any of
    // them is legible in both, so the chip needs no knowledge of the page.
    //
    // Reverse video (SGR 7) was the first attempt and is not used: it would
    // let the terminal invert its own two colours, which is tidier in theory,
    // but it renders as plain bold text anywhere SGR 7 is unimplemented and
    // there is then no chip at all. An explicit background always paints one.
    // BRIGHT backgrounds (100-107) with BLACK ink. All four bright hues are
    // light enough that black reads on every one of them; white is not — it
    // fails on bright yellow, which is the chip that says NUDGE. One ink for
    // five chips means the worst pairing decides, so it has to be the safe one.
    // The chips are TRUECOLOR even here, and that is not an inconsistency
    // with the terminal-relative text above — it follows from the same rule.
    // Body text sits on the page, so it must defer to a background it was
    // never told. A chip paints its OWN ground, so the only contrast that
    // matters is ink against chip, which is fully known. Deferring here
    // instead bought nothing and cost the guarantee: indexed backgrounds are
    // rendered bright by some terminals and dim by others, and bold is widely
    // taken as "use the bright variant", so black ink came out grey on a
    // yellow that came out olive.
    ink: "\x1b[38;2;250;250;250m",
    bgOut: "\x1b[48;2;21;101;177m",
    bgIn: "\x1b[48;2;22;120;62m",
    bgWarn: "\x1b[48;2;166;90;12m",
    bgErr: "\x1b[48;2;178;38;38m",
    bgMuted: "\x1b[48;2;90;94;110m",
  },
};

// Only "light" and "dark" are answers. "auto", "" and anything else mean no
// opinion, matching how magmux itself reads the same variable — one word for
// one meaning across the two processes.
function resolvePalette(v: string | undefined): Palette {
  const word = (v ?? "").trim().toLowerCase();
  return PALETTES[word === "light" || word === "dark" ? word : "unknown"];
}

const C = resolvePalette(process.env.MAGMUX_THEME);

const stamp = () => new Date().toTimeString().slice(0, 8);

// The pane is narrow and shares a window with the session it is driving, so
// the width is read once and every body is wrapped to it. COLUMNS is what
// magmux exports for exactly this; 72 is a sane floor for a pipe.
const COLS = Math.max(Number(process.env.COLUMNS) || 0, 40) || 72;

// badge renders a status chip: ink on a saturated block.
//
// A pilot pane is scanned between glances at the session beside it, so what it
// has to answer in one look is "which of these lines is an instruction, which
// is a result, and did anything fail". Coloured text answers that only after
// it is read. A filled chip answers it as a shape.
const badge = (label: string, bgCol: string) =>
  `${bgCol}${C.ink}${C.bold} ${label} ${C.reset}`;

// wrap breaks a body to the pane, never mid-word where it can be helped.
function wrap(text: string, width: number): string[] {
  const out: string[] = [];
  for (const para of text.split("\n")) {
    let line = "";
    for (const word of para.split(/\s+/).filter(Boolean)) {
      if (!line) line = word;
      else if (line.length + 1 + word.length <= width) line += " " + word;
      else {
        out.push(line);
        line = word;
      }
    }
    out.push(line);
  }
  return out.length ? out : [""];
}

// log writes one entry: a head row, then the body under a direction-tinted
// rule.
//
// The rule is the whole point of the shape. The stream interleaves two
// provenances — instructions this pilot SENT and turns magmux OBSERVED — and
// before it, a body was an unmarked block of grey that belonged to whichever
// head happened to precede it. Now the body is visibly attached to its head
// and carries its colour down the left, so a wall of blue with no green in it
// reads as "the session has stopped answering" without a word being read.
const GUTTER = "  \u258e "; // two spaces, a left one-quarter block, a space
const log = (color: string, glyph: string, head: string, body = "", tail = "") => {
  let s = `${C.grey}${stamp()}${C.reset} ${color}${glyph}${C.reset} ${head}`;
  if (tail) s += ` ${C.grey}${tail}${C.reset}`;
  s += "\n";
  if (body) {
    for (const line of wrap(body, COLS - GUTTER.length - 1)) {
      s += `${color}${GUTTER}${C.reset}${C.text}${line}${C.reset}\n`;
    }
  }
  process.stdout.write(s);
};

// rule draws a full-width separator, so the run's phases do not run together.
const rule = (label = "") => {
  const text = label ? ` ${label} ` : "";
  const bar = "\u2500".repeat(Math.max(COLS - text.length - 1, 0));
  process.stdout.write(`${C.grey}${text}${bar}${C.reset}\n`);
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

  rule("PILOT");
  log(C.out, badge("GOAL", C.bgOut), `${C.grey}driving pane${C.reset} ${args.pane}`, args.goal);

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
  log(C.grey, "·", `${C.grey}model${C.reset} ${modelName}`);

  // Wait for the session to be ready for its first instruction. Sending into
  // a still-booting session would land the text in a TUI that has not drawn
  // its input box yet, and be silently lost.
  log(C.grey, "·", "waiting for the session to come up…");
  if (!(await bridge.waitUntilReady())) {
    bridge.finish("session never became ready", true);
    log(C.err, badge("NO SESSION", C.bgErr), "never reached awaiting_input");
    bridge.close();
    process.exit(1);
  }
  log(C.in, badge("READY", C.bgIn), `${C.grey}session is accepting input${C.reset}`);

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
      log(C.out, badge(label, C.bgOut), "", params.instruction);

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

  // A request the provider REFUSED is recorded by pi as a finished assistant
  // message carrying stopReason "error" — session.prompt() returns normally and
  // throws nothing. Watching only text_delta therefore misses it completely,
  // and the run then presents as a model that would not call its tools: three
  // empty turns, two nudges, and the summary "the pilot stopped without calling
  // finish". Diagnosed once against an ANTHROPIC_API_KEY with no credit
  // balance, where the truth was a 400 on every single request and the pilot
  // never had a turn to misbehave in.
  let providerError = "";
  session.subscribe((event) => {
    if (event.type === "message_update" && event.assistantMessageEvent.type === "text_delta") {
      process.stdout.write(C.grey + event.assistantMessageEvent.delta + C.reset);
      return;
    }
    const msg = (event as { message?: { stopReason?: string; errorMessage?: string } }).message;
    if (msg?.stopReason === "error" && msg.errorMessage && !providerError) {
      providerError = describeProviderError(msg.errorMessage);
      log(C.err, badge("PROVIDER", C.bgErr), "refused the request", providerError);
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
      // Nudging a provider that refused the request buys nothing but two more
      // refusals and a summary that blames the model for the account.
      if (providerError) break;
      if (attempt >= NUDGE_LIMIT) break;
      log(C.warn, badge("NUDGE", C.bgWarn), "replied without calling a tool");
      prompt =
        `You did not call a tool, so nothing happened. Your reply is not visible ` +
        `to the session. Call send_to_session now with the next instruction, or ` +
        `call finish if the goal is already met.`;
    }
  } finally {
    session.dispose();
  }

  let summary = done ?? {
    summary: providerError
      ? `the pilot could not reach its model: ${providerError}`
      : "the pilot stopped without calling finish",
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
  // The closing statement is the one line someone scrolls back to find, so it
  // gets a rule above it and the run's only full-width chip.
  rule();
  log(
    summary.success ? C.in : C.err,
    badge(summary.success ? "FINISHED" : "FAILED", summary.success ? C.bgIn : C.bgErr),
    `${C.grey}${sent} instruction${sent === 1 ? "" : "s"} sent${C.reset}`,
    summary.summary,
  );

  // Give the socket a moment to flush the finish before the process exits and
  // takes the connection with it.
  await new Promise((r) => setTimeout(r, 200));
  bridge.close();
  process.exit(summary.success ? 0 : 1);
}

/** Turn a TurnResult into the text the pilot model reads. */
// describeProviderError turns pi's raw errorMessage into one readable line.
//
// The raw value is the HTTP status followed by the provider's JSON body, e.g.
// `400 {"type":"error","error":{"type":"invalid_request_error","message":"Your
// credit balance is too low..."},"request_id":"req_011C..."}`. The sentence a
// human needs is buried two levels in, and printing the whole body pushes it
// off the pane it has to be read in. Falls back to the raw string whenever the
// shape is not what we expect — a provider we have never seen must still be
// able to say what went wrong.
function describeProviderError(raw: string): string {
  const brace = raw.indexOf("{");
  const status = brace > 0 ? raw.slice(0, brace).trim() : "";
  if (brace >= 0) {
    try {
      const body = JSON.parse(raw.slice(brace));
      const message = body?.error?.message ?? body?.message;
      if (typeof message === "string" && message) {
        return status ? `${status} ${message}` : message;
      }
    } catch {
      // Not JSON, or truncated. The raw string below is still the best answer.
    }
  }
  return raw.replace(/\s+/g, " ").slice(0, 200);
}

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
      log(C.in, badge("AWAITING", C.bgIn), r.tool ? `${C.grey}last tool${C.reset} ${r.tool}` : "", r.response, secs);
      break;
    case "awaiting_permission":
      log(C.warn, badge("BLOCKED", C.bgWarn), "permission required", r.response, secs);
      break;
    case "error":
      log(C.err, badge("ERROR", C.bgErr), "", r.response, secs);
      break;
    case "gone":
      log(C.err, badge("GONE", C.bgErr), "session exited", "", secs);
      break;
    default:
      log(C.warn, badge("STALLED", C.bgWarn), "", "no turn started", secs);
  }
}

main().catch((e) => {
  console.error(`${C.err}pilot: ${e?.stack ?? e}${C.reset}`);
  process.exit(1);
});
