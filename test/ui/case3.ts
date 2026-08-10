#!/usr/bin/env bun
/**
 * UI case 3 — a controlled session: a pi.dev agent guides a real Claude Code
 * session through a multi-step task, and you watch it happen.
 *
 * magmux takes over THIS terminal and renders three panes: the Claude Code
 * session being driven, the pi pilot's own reasoning, and magmux's control
 * panel showing the traffic between them. The whole point of the case is that
 * it is watchable — the panel should visibly fill with ▶ instructions and ◀
 * results as the run progresses.
 *
 * What makes this a real test rather than a demo is the ground truth. Both
 * agents *report* success in their own words, and either could be wrong or
 * simply lying. So the task produces a file on disk, and the final assertion
 * reads that file. A run where both agents declare victory and the file is
 * missing or misordered fails.
 *
 * It also pins the contract the wiring exists for:
 *   - the pilot's instructions arrive as `control` dir=out events
 *   - those instructions are briefs, not shell commands — the pilot must
 *     delegate to the session, not use it as a terminal
 *   - the session's own reply text reaches the pilot (it once did not: a
 *     transcript-discovery bug left every turn textless)
 *   - the panel's completions come from magmux's own observation, so the
 *     count of observed turns cannot exceed the turns that actually happened
 *
 * Note it does NOT assert a minimum step count. A well-briefed pilot may
 * accomplish a small goal in one instruction and let the session decompose it
 * — that is the desired behaviour, and demanding "multi-step" would push it
 * back to relaying one command per turn.
 *
 * Costs real Claude Code AND real pi usage. Needs a provider configured for
 * pi (see https://pi.dev) plus Claude Code auth.
 */
import { execSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import {
  C, Check, InlineMagmuxRun, SnapshotEvent, findSocket, header,
  isLiveSnapshot, report, sleep, streamEvents, strippedMarkers, truncate,
} from "./harness.ts";

const REPO = path.resolve(import.meta.dir, "../..");
const BIN = process.env.MAGMUX_BIN ? path.resolve(process.env.MAGMUX_BIN) : path.join(REPO, "magmux");
const PILOT = path.join(REPO, "pilot", "pilot.ts");
const HOLD_MS = Number(process.env.HOLD_MS ?? 6000);
const TIMEOUT_MS = Number(process.env.TIMEOUT_MS ?? 600_000);

/** Ground truth the run must produce on disk. */
const LINES = ["1", "4", "9", "16", "25"];
const ARTIFACT = "squares.txt";

/**
 * A task, not a plan. Earlier versions of this goal spelled out the
 * decomposition ("add ONE line per instruction, show the file back after
 * each"), which meant the pilot was executing someone else's plan and the run
 * only looked autonomous. This states an outcome and says nothing about how to
 * get there: writing the script, running it, and checking the result are
 * milestones the pilot has to work out for itself.
 *
 * Deliberately no --steps either — naming a count is handing over the plan.
 */
const GOAL =
  `Set up a tiny command-line tool in this directory: a script that prints the ` +
  `squares of 1 through N, where N is its first argument; a README.md explaining ` +
  `how to run it; and ${ARTIFACT} holding the output for N=5, one value per line ` +
  `and nothing else. Make sure it actually runs and the file really holds the ` +
  `right values.`;

header(
  "magmux UI case 3 — a pi.dev pilot guiding a live Claude Code session",
  "watch the control panel: ▶ instructions out, ◀ observed results back",
);

// --- preflight -------------------------------------------------------------

console.log(`\n${C.bold}  Setup${C.reset}`);

if (!InlineMagmuxRun.supported()) {
  console.error(
    `\n  ${C.red}This case renders the magmux UI into your terminal, so it needs a TTY.${C.reset}\n` +
      `  ${C.grey}Run it directly (not piped or redirected):  bun run test:ui:case3${C.reset}\n`,
  );
  process.exit(2);
}

// Fail early and legibly when pi has no usable provider, rather than letting
// the pilot die inside a pane where its error scrolls past unread.
let pilotModel = process.env.PILOT_MODEL ?? "";
try {
  const { ModelRuntime } = await import("@earendil-works/pi-coding-agent");
  const runtime = await ModelRuntime.create();
  const available = await runtime.getAvailable();
  if (available.length === 0) {
    console.error(
      `\n  ${C.red}pi has no model available — the pilot cannot run.${C.reset}\n` +
        `  ${C.grey}Configure a provider for pi (see https://pi.dev), e.g. export ANTHROPIC_API_KEY,${C.reset}\n` +
        `  ${C.grey}then re-run. Set PILOT_MODEL=provider/id to pick a specific one.${C.reset}\n`,
    );
    process.exit(2);
  }
  if (!pilotModel) {
    // The pilot does no coding, but it does have to track what has and has
    // not been done across turns — and that is where small models fall down.
    // Measured on this exact task: haiku-4-5 spent six of eight steps
    // re-checking state it had already confirmed, ran out of budget after two
    // of three lines, and then reported all three as done. Prefer a
    // mid-tier model; the run is short enough that the cost difference is
    // small next to a wasted run.
    const preferred = ["anthropic/claude-sonnet-5", "anthropic/claude-sonnet-4-5", "openai/gpt-5"];
    const byName = new Map(available.map((m) => [`${m.provider}/${m.id}`, m]));
    pilotModel = preferred.find((n) => byName.has(n)) ?? `${available[0].provider}/${available[0].id}`;
  }
  console.log(`  ${C.grey}pilot  ${C.reset}${pilotModel} ${C.grey}(${available.length} model(s) available)${C.reset}`);
} catch (e) {
  console.error(`\n  ${C.red}could not load pi: ${String(e)}${C.reset}`);
  console.error(`  ${C.grey}Install it with: npm install${C.reset}\n`);
  process.exit(2);
}

// A scratch cwd, so the run cannot touch the repo and the artifact check is
// unambiguous — the file either exists there because this run made it, or it
// does not exist at all.
const workdir = fs.mkdtempSync(path.join(os.tmpdir(), "magmux-case3-"));
const artifactPath = path.join(workdir, ARTIFACT);

console.log(`  ${C.grey}cwd    ${C.reset}${workdir}`);
console.log(`  ${C.grey}task   ${C.reset}script → ${ARTIFACT} = [${LINES.join(", ")}]  ${C.grey}(pilot plans its own steps)${C.reset}`);
console.log(`  ${C.grey}env    ${C.reset}stripping ${strippedMarkers().length} Claude Code marker(s)`);

if (!process.env.MAGMUX_BIN) {
  execSync(`go build -o ${JSON.stringify(BIN)} .`, { cwd: REPO, stdio: "inherit" });
  console.log(`  ${C.green}✓${C.reset} built ${path.relative(REPO, BIN)}`);
} else {
  console.log(`  ${C.yellow}!${C.reset} MAGMUX_BIN override ${C.grey}${BIN}${C.reset}`);
}

// --- run -------------------------------------------------------------------

// Pane 0 is the driven session, pane 1 the pilot, and -c appends the control
// panel as pane 2. The pilot finds the socket through MAGMUX_SOCK, which
// magmux exports to every pane it spawns.
const pilotCmd =
  `bun ${JSON.stringify(PILOT)} --pane 0 --model ${JSON.stringify(pilotModel)} ` +
  `--max-steps 10 --goal ${JSON.stringify(GOAL)}`;

const args = [
  "-c",
  "-w",
  "-e", "claude --dangerously-skip-permissions",
  "-e", pilotCmd,
];

console.log(
  `\n  ${C.cyan}Handing the terminal to magmux — watch the control panel.${C.reset}` +
    ` ${C.grey}Verdict prints when it exits.${C.reset}`,
);
await sleep(1200);

const run = new InlineMagmuxRun(BIN, args, workdir);
const sessionStates: string[] = [];      // state transitions of pane 0
const sessionResponses: string[] = [];   // reply text magmux observed on pane 0
const outbound: SnapshotEvent[] = [];    // control dir=out (pilot → session)
const notes: SnapshotEvent[] = [];       // control dir=note (start / finish)
const timeline: string[] = [];
let finalResults: SnapshotEvent | null = null;
let socketErr: unknown = null;

run.start();
try {
  const sock = await findSocket(run.startedAt);
  await streamEvents(sock, (ev, t) => {
    const at = `  ${C.grey}${t.toFixed(1).padStart(6)}s${C.reset}`;

    if (ev.type === "control") {
      if (ev.dir === "out") {
        outbound.push(ev);
        timeline.push(
          `${at}  ${C.blue}▶ ${String(ev.label ?? "send").padEnd(12)}${C.reset}` +
            `"${truncate(String(ev.text ?? ""), 46)}"`,
        );
      } else if (ev.dir === "note") {
        notes.push(ev);
        timeline.push(
          `${at}  ${C.magenta}• ${String(ev.event).padEnd(12)}${C.reset}` +
            `${truncate(String(ev.goal ?? ev.summary ?? ""), 46)}`,
        );
      }
      return;
    }

    if (ev.type === "results") {
      finalResults = ev;
      return;
    }

    if (!isLiveSnapshot(ev) || ev.pane !== 0) return;
    const state = String(ev.state);
    if (sessionStates[sessionStates.length - 1] !== state) sessionStates.push(state);
    const resp = String(ev.response ?? "").trim();
    if (resp) sessionResponses.push(resp);
    timeline.push(
      `${at}  ${state === "awaiting_input" ? C.green + "◀" : C.yellow + "◐"} ` +
        `${state.padEnd(12)}${C.reset}` +
        (resp ? `${C.dim}resp=${C.reset}"${truncate(resp, 40)}"` : `${C.grey}—${C.reset}`),
    );
    return run.hasExited || undefined;
  }, TIMEOUT_MS);

  for (let i = 0; i < HOLD_MS / 100 && !run.hasExited; i++) await sleep(100);
} catch (e) {
  socketErr = e;
} finally {
  await run.stop();
}

// --- report (terminal is ours again) --------------------------------------

console.log(`\n${C.bold}  Timeline${C.reset}\n`);
for (const row of timeline) console.log(row);
if (socketErr) console.log(`\n  ${C.red}socket error: ${String(socketErr)}${C.reset}`);

// Ground truth: what is actually on disk, independent of either agent's claims.
let artifact: string[] | null = null;
try {
  artifact = fs.readFileSync(artifactPath, "utf8").split("\n").map((l) => l.trim()).filter(Boolean);
} catch {
  artifact = null;
}
let produced: string[] = [];
try {
  produced = fs.readdirSync(workdir).sort();
} catch {}

console.log(`\n${C.bold}  Artifacts${C.reset} ${C.grey}${workdir}${C.reset}`);
console.log(`  ${C.grey}files: ${produced.join(", ") || "(none)"}${C.reset}`);
console.log(
  artifact
    ? `  ${artifact.map((l) => `${C.green}${l}${C.reset}`).join(`${C.grey} · ${C.reset}`)}`
    : `  ${C.red}(not created)${C.reset}`,
);

const finish = notes.find((n) => n.event === "finish" || n.event === "fail");
const turns = sessionStates.filter((s) => s === "awaiting_input").length;
const panelEntry = (finalResults?.panes as Array<Record<string, unknown>> | undefined)
  ?.find((p) => p.control === true);

const checks: Check[] = [
  {
    label: "pilot announced itself to magmux",
    pass: notes.some((n) => n.event === "start"),
    detail: notes.some((n) => n.event === "start") ? "control note event=start" : "no start note",
  },
  {
    // Deliberately >= 1, not >= 2. A pilot briefed to delegate outcomes will
    // often accomplish a small goal in a single instruction and let the
    // session decompose it internally — which is the better behaviour, not a
    // failure. Asserting "multi-step" here would punish exactly the change we
    // want and push the pilot back towards relaying one command per turn.
    label: "pilot drove the session",
    pass: outbound.length >= 1 && turns >= 1,
    detail: `${outbound.length} instruction(s), ${turns} completed turn(s): ${sessionStates.join(" → ") || "none"}`,
  },
  {
    // The behaviour the prompt rework is for: instructions must be briefs, not
    // shell. Catching `&&`, redirects and pipes is crude but it is exactly
    // what regression looks like — the old pilot sent `echo "alpha" >
    // relay.txt && cat relay.txt`.
    label: "instructions are briefs, not shell commands",
    pass: outbound.every((ev) => !/(^|\s)(run:|echo |cat |ls |grep )|&&|\s>\s|\s\|\s/.test(String(ev.text ?? ""))),
    detail: (() => {
      const bad = outbound.find((ev) =>
        /(^|\s)(run:|echo |cat |ls |grep )|&&|\s>\s|\s\|\s/.test(String(ev.text ?? "")));
      return bad ? `shell-shaped: "${truncate(String(bad.text), 44)}"` : "all plain-language";
    })(),
  },
  {
    // Regression guard for the transcript-discovery bug: the controller once
    // never locked onto the JSONL at all, so every turn came back with no
    // text and the pilot flew blind, burning steps re-asking for output.
    label: "session's own text reached the pilot",
    pass: sessionResponses.some((r) => r.trim().length > 0),
    detail: sessionResponses.length
      ? `longest reply ${Math.max(...sessionResponses.map((r) => r.length))} chars`
      : "no response text observed at all",
  },
  {
    // Every observed completion must be preceded by an instruction. More
    // completions than instructions would mean the panel is counting turns
    // the pilot never asked for.
    label: "observed turns never outnumber instructions",
    pass: turns <= outbound.length + 1,
    detail: `${turns} turn(s) vs ${outbound.length} instruction(s)`,
  },
  {
    label: "pilot declared the run finished",
    pass: !!finish && finish.event === "finish" && finish.failed !== true,
    detail: finish
      ? `${finish.event}: "${truncate(String(finish.summary ?? ""), 40)}"`
      : "pilot never called finish",
  },
  {
    label: "control panel reported as a panel, not a stuck session",
    pass: panelEntry?.state === "panel",
    detail: panelEntry ? `state=${panelEntry.state}` : "no control entry in results",
  },
  // The assertions that cannot be satisfied by either agent's self-report.
  {
    label: `${ARTIFACT} exists on disk with the right lines in order`,
    pass: !!artifact && artifact.length === LINES.length && artifact.every((l, i) => l === LINES[i]),
    detail: artifact ? `got [${artifact.join(", ")}] want [${LINES.join(", ")}]` : "file missing",
  },
  {
    // The task named three deliverables; a run that produced only the data
    // file did part of the job while reporting all of it.
    label: "every deliverable the task named exists",
    pass: produced.some((f) => /^readme\.md$/i.test(f)) &&
      produced.some((f) => /\.(py|sh|js|ts|rb|pl)$/i.test(f)),
    detail: produced.length ? produced.join(", ") : "(nothing created)",
  },
];

const code = report(checks);
console.log(`  ${C.grey}workdir kept for inspection: ${workdir}${C.reset}\n`);
process.exit(code);
