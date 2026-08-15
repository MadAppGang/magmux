#!/usr/bin/env bun
/**
 * UI case 4 — the MCP controlled session: a real Claude Code agent drives
 * another real Claude Code session through `magmux mcp`, and you watch it.
 *
 * This is case 3 with the pilot replaced by the flagship MCP path. magmux takes
 * over THIS terminal and renders three panes: the session being driven (pane
 * 0), the driver (pane 1), and magmux's control panel (pane 2) showing the
 * traffic between them. As in case 3, the point is that it is watchable — the
 * panel should visibly fill with ▶ instructions and ◀ observed results.
 *
 * What it demonstrates that case 3 cannot: the **host session** acquisition
 * path. magmux exports MAGMUX_SOCK to every pane it spawns; Claude Code passes
 * its environment to the MCP servers it spawns; so `magmux mcp`, started by the
 * driver in pane 1, auto-attaches to the very magmux it is running inside. The
 * human never wires anything up, and watches the whole time.
 *
 * What makes this a test rather than a demo is the ground truth. Both agents
 * *report* success in their own words and either could be wrong or simply
 * lying, so the task produces files on disk and the final assertion reads them:
 *
 *   - `squares.txt` must exist in **pane 0's** workdir with the exact lines in
 *     order, alongside a README.md and a script;
 *   - the driver runs with `--disallowedTools Bash,Write,Edit` from a
 *     *different* cwd, so the only route to that file is through the driven
 *     pane — and we assert its own cwd is empty of the artifact;
 *   - the instructions must appear as magmux's own `control` dir=out events, so
 *     they went through the `send` path rather than the driver shelling out;
 *   - observed completions can never outnumber the instructions, because OUT
 *     and IN come from different sources (CLAUDE.md's provenance invariant).
 *
 * And one assertion case 3 has no use for: at least one signal that only the
 * new request/response layer produces — a `pane_opened` broadcast, an `ok`
 * reply, an MCP tool name riding the `label` field, or the `client` identity on
 * the start note. Without it a green run could in principle have come from the
 * legacy fire-and-forget verbs alone.
 *
 * Costs real Claude Code usage TWICE (driver and driven) and needs Claude Code
 * auth. Not part of `go test` for exactly that reason.
 *
 * A note on the driver pane, because it is the one thing a reader will
 * immediately ask about. `claude -p` is print mode: with the default text
 * output format it emits NOTHING until the whole run is over, so the driver's
 * half of the screen sat blank for the entire case — in a *visual* case, the
 * one pane you most want to watch. Claude Code 2.1.232 will instead stream
 * line-delimited JSON (`--output-format stream-json --verbose`), one object per
 * message, as it happens; that is raw and unreadable, so the pane pipes it
 * through DRIVER_STREAM_FMT below, which renders one short line per event
 * (`→ send_and_wait(pane 0)`, `← pane 0 · awaiting_input · 62s`). Nothing about
 * the verdict changes: the formatter only reads stdout, and the driver keeps
 * the same flags, the same cwd and the same missing tools.
 *
 * The 50/50 split of the left column is not a choice this file gets to make:
 * with three panes `buildGrid` gives the left column ceil(3/2) = 2 panes and
 * `buildColumn` splits it evenly, and the control panel is always appended
 * last, so it is always the whole right column. Ordering the panes differently
 * moves them around but cannot give the driven session more rows. That is
 * another argument for the formatter — the driver's half is fixed, so the best
 * available fix is to make it worth looking at.
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
const HOLD_MS = Number(process.env.HOLD_MS ?? 6000);
const TIMEOUT_MS = Number(process.env.TIMEOUT_MS ?? 600_000);

/** Ground truth the run must produce on disk, in the DRIVEN pane's workdir. */
const LINES = ["1", "4", "9", "16", "25"];
const ARTIFACT = "squares.txt";

/**
 * Quote one token for `$SHELL -l -c`, which is how magmux runs an `-e` line.
 * Single quotes because the goal text must reach claude verbatim — a `$` or a
 * backtick inside a double-quoted line would be expanded by the login shell
 * before claude ever saw it.
 */
const shq = (s: string) => `'${s.replace(/'/g, `'\\''`)}'`;

/**
 * The driver pane's stdout filter, written to a temp file at startup and run
 * with the very bun that is running this case (an absolute path, so a login
 * shell with a different PATH cannot miss it).
 *
 * It reads Claude Code's `--output-format stream-json` and prints one line per
 * event. It is deliberately dumb: anything it does not recognise is printed
 * verbatim rather than swallowed, because a filter that hides the one line
 * explaining why a run failed is worse than no filter at all. `2>&1` sends
 * claude's stderr through here too, so warnings keep their place in the order.
 *
 * While a tool call is in flight it repaints a `⋯ send_and_wait 45s` heartbeat
 * in place — send_and_wait blocks for the whole of the driven session's turn,
 * which is minutes, and a pane that prints nothing for minutes is the exact
 * problem this file is fixing.
 *
 * String.raw so the `\x1b` escapes reach the written file as source text; the
 * body therefore contains no backtick and no `${`, which would end or
 * interpolate this literal.
 */
const DRIVER_STREAM_FMT = String.raw`#!/usr/bin/env bun
// Generated by test/ui/case4.ts — renders Claude Code stream-json as one line
// per event. Deleted when the case exits.
const C = {
  r: "\x1b[0m", d: "\x1b[2m", grey: "\x1b[90m", cyan: "\x1b[36m", blue: "\x1b[34m",
  green: "\x1b[32m", yellow: "\x1b[33m", red: "\x1b[31m", magenta: "\x1b[35m",
};
const T0 = Date.now();
const cols = () => (process.stdout.columns > 20 ? process.stdout.columns : 60);
const el = () => ((Date.now() - T0) / 1000).toFixed(0).padStart(4) + "s";
const flat = (s) => String(s == null ? "" : s).replace(/\s+/g, " ").trim();
const clip = (s, n) => {
  const f = flat(s);
  return f.length > n ? f.slice(0, n - 1) + "…" : f;
};

let pending = null;
let beat = null;

function clearBeat() {
  if (beat) { clearInterval(beat); beat = null; }
  if (process.stdout.isTTY) process.stdout.write("\r\x1b[2K");
}

// One event, one line: elapsed, a coloured mark, a short head, a dim tail that
// is clipped to whatever room the pane's width leaves.
function say(mark, color, head, tail) {
  clearBeat();
  const lead = C.grey + el() + " " + C.r + color + mark + " " + head + C.r;
  const room = cols() - (el().length + 3 + flat(head).length + 2);
  console.log(lead + (tail && room > 12 ? "  " + C.d + clip(tail, room) + C.r : ""));
}

function startBeat(name) {
  pending = { name: name, at: Date.now() };
  if (!process.stdout.isTTY) return;
  beat = setInterval(() => {
    const s = ((Date.now() - pending.at) / 1000).toFixed(0);
    process.stdout.write("\r\x1b[2K" + C.grey + el() + " " + C.yellow +
      "⋯ " + pending.name + " " + s + "s" + C.r);
  }, 5000);
  if (beat.unref) beat.unref();
}

function handle(line) {
  if (!line) return;
  let ev;
  try {
    ev = JSON.parse(line);
  } catch {
    clearBeat();
    console.log(C.grey + line + C.r); // not ours — show it whole, unclipped
    return;
  }

  if (ev.type === "system") {
    if (ev.subtype !== "init") return; // hook_started/hook_response/etc: noise
    const mcp = (ev.mcp_servers || []).map((s) => s.name + ":" + s.status).join(" ");
    say("●", C.cyan, "session", (ev.model || "") + (mcp ? "   mcp " + mcp : ""));
    return;
  }

  if (ev.type === "assistant") {
    for (const b of (ev.message && ev.message.content) || []) {
      if (b.type === "thinking") {
        say("◦", C.grey, "thinking", b.thinking);
      } else if (b.type === "text" && flat(b.text)) {
        say("·", C.r, "says", b.text);
      } else if (b.type === "tool_use") {
        const name = String(b.name || "tool").replace(/^mcp__[A-Za-z0-9_]+__/, "");
        const a = b.input || {};
        const where = a.pane === undefined ? "" : "(pane " + a.pane + ")";
        const what = a.instruction || a.text || a.cmd || a.command ||
          (Array.isArray(a.keys) ? a.keys.join(" ") : "");
        say("→", C.blue, name + where, what);
        startBeat(name);
      }
    }
    return;
  }

  if (ev.type === "user") {
    for (const b of (ev.message && ev.message.content) || []) {
      if (b.type !== "tool_result") continue;
      const raw = typeof b.content === "string" ? b.content
        : Array.isArray(b.content)
          ? b.content.map((x) => (x && x.text ? x.text : "")).join(" ")
          : "";
      const rows = String(raw).split("\n");
      // magmux's own tools answer with "pane 0 · awaiting_input · 62s" on the
      // first line, which is exactly the summary this row wants.
      const head = clip(rows.find((l) => l.trim()) || "(no output)", 44);
      const tail = rows.slice(1).join(" ");
      if (b.is_error) say("✗", C.red, "error", head + " " + tail);
      else say("←", C.green, head, tail);
    }
    return;
  }

  if (ev.type === "result") {
    const ok = ev.is_error !== true && ev.subtype === "success";
    say(ok ? "✔" : "✗", ok ? C.green : C.red, ok ? "done" : "failed",
      (ev.num_turns || 0) + " turns · " +
      ((ev.duration_ms || 0) / 1000).toFixed(0) + "s · $" +
      Number(ev.total_cost_usd || 0).toFixed(2) +
      (ev.result ? "   " + flat(ev.result) : ""));
    return;
  }
}

// Print something before the first event so the pane is never blank: claude
// takes a few seconds to boot its MCP server and say anything at all.
say("●", C.magenta, "driver", "claude -p, driving a session over the magmux MCP tools");

const dec = new TextDecoder();
let buf = "";
for await (const chunk of Bun.stdin.stream()) {
  buf += dec.decode(chunk, { stream: true });
  let nl;
  while ((nl = buf.indexOf("\n")) >= 0) {
    const line = buf.slice(0, nl);
    buf = buf.slice(nl + 1);
    handle(line.trim());
  }
}
handle(buf.trim());
clearBeat();
`;

/**
 * The task the DRIVEN session has to accomplish. Word-for-word case 3's goal,
 * so the two cases stay comparable: same deliverables, same artifact, same
 * "make sure it actually runs" clause, and — deliberately — no decomposition.
 */
const TASK =
  `Set up a tiny command-line tool in that directory: a script that prints the ` +
  `squares of 1 through N, where N is its first argument; a README.md explaining ` +
  `how to run it; and ${ARTIFACT} holding the output for N=5, one value per line ` +
  `and nothing else. Make sure it actually runs and the file really holds the ` +
  `right values.`;

/**
 * The goal handed to the DRIVER. It states an outcome and names the tools, and
 * says nothing about how many instructions to send or how to break the work up
 * — the same reasoning as case 3's goal comment: spelling out the plan would
 * make the run only look autonomous.
 *
 * The "do not do it yourself" paragraph is belt and braces. The real guarantee
 * is enforced by the CLI flags (no Bash, no Write, no Edit, different cwd) and
 * checked on disk afterwards; the prose only stops the driver wasting turns
 * discovering that its tools are gone.
 */
const DRIVER_GOAL =
  `You are driving another terminal session through the magmux MCP tools. ` +
  `Call list_panes first: one of the panes is a Claude Code session sitting in ` +
  `its own working directory, and that session is the one you must drive (the ` +
  `tools refuse your own pane and the control panel, so pick the pane they let ` +
  `you send to). Use send_and_wait to instruct it, in plain language, to do ` +
  `this: ${TASK} ` +
  `Then verify by asking that session to show you the contents of ${ARTIFACT}, ` +
  `and use read_pane if a turn comes back with nothing to say. ` +
  `You must NOT do any of this work yourself: you have no shell and no file ` +
  `tools, and you are in a different directory, so the only way to succeed is ` +
  `to get the other session to do it. When the files exist and hold the right ` +
  `values, reply DONE and stop.`;

header(
  "magmux UI case 4 — a Claude Code agent driving a live session over MCP",
  "watch the control panel: ▶ MCP tool calls out, ◀ observed results back",
);

// --- preflight -------------------------------------------------------------

console.log(`\n${C.bold}  Setup${C.reset}`);

if (!InlineMagmuxRun.supported()) {
  console.error(
    `\n  ${C.red}This case renders the magmux UI into your terminal, so it needs a TTY.${C.reset}\n` +
      `  ${C.grey}Run it directly (not piped or redirected):  bun run test:ui:case4${C.reset}\n`,
  );
  process.exit(2);
}

// Both panes are Claude Code here — the driver AND the driven — so a missing
// `claude` is not a slow failure inside a pane, it is two of them.
let claudeBin = "";
try {
  claudeBin = execSync("command -v claude", { encoding: "utf8" }).trim();
} catch {
  claudeBin = "";
}
if (!claudeBin) {
  console.error(
    `\n  ${C.red}claude is not on PATH — this case needs it twice (driver and driven).${C.reset}\n` +
      `  ${C.grey}Install Claude Code and make sure \`claude\` runs, then re-run.${C.reset}\n`,
  );
  process.exit(2);
}
console.log(`  ${C.grey}claude ${C.reset}${claudeBin}`);

// Two scratch directories, and the separation is load-bearing. The driven
// session works in `workdir`; the driver runs in `driverDir` and must leave it
// empty. A single shared cwd would make "the driver could not have written it"
// an argument about CLI flags alone.
const workdir = fs.mkdtempSync(path.join(os.tmpdir(), "magmux-case4-driven-"));
const driverDir = fs.mkdtempSync(path.join(os.tmpdir(), "magmux-case4-driver-"));
const artifactPath = path.join(workdir, ARTIFACT);

// The MCP config the driver is launched with. It points at THIS binary, so the
// case exercises the build under test rather than whatever `claude mcp add`
// left in the user's config.
const configPath = path.join(os.tmpdir(), `magmux-case4-mcp-${process.pid}.json`);
fs.writeFileSync(
  configPath,
  JSON.stringify({ mcpServers: { magmux: { command: BIN, args: ["mcp"] } } }) + "\n",
);
// The driver's stdout filter, beside the config and cleaned up with it.
const streamPath = path.join(os.tmpdir(), `magmux-case4-stream-${process.pid}.ts`);
fs.writeFileSync(streamPath, DRIVER_STREAM_FMT);
const cleanupTemp = () => {
  try { fs.unlinkSync(configPath); } catch {}
  try { fs.unlinkSync(streamPath); } catch {}
};

console.log(`  ${C.grey}driven ${C.reset}pane 0 in ${workdir}`);
console.log(`  ${C.grey}driver ${C.reset}pane 1 in ${driverDir} ${C.grey}(no Bash/Write/Edit)${C.reset}`);
console.log(`  ${C.grey}mcp    ${C.reset}${configPath} ${C.grey}→ ${BIN} mcp${C.reset}`);
console.log(`  ${C.grey}stream ${C.reset}${streamPath} ${C.grey}(stream-json → one line per event)${C.reset}`);
console.log(`  ${C.grey}task   ${C.reset}script → ${ARTIFACT} = [${LINES.join(", ")}]  ${C.grey}(driver plans its own steps)${C.reset}`);
console.log(`  ${C.grey}env    ${C.reset}stripping ${strippedMarkers().length} Claude Code marker(s)`);

if (!process.env.MAGMUX_BIN) {
  execSync(`go build -o ${JSON.stringify(BIN)} .`, { cwd: REPO, stdio: "inherit" });
  console.log(`  ${C.green}✓${C.reset} built ${path.relative(REPO, BIN)}`);
} else {
  console.log(`  ${C.yellow}!${C.reset} MAGMUX_BIN override ${C.grey}${BIN}${C.reset}`);
}

// --- run -------------------------------------------------------------------

// Pane 0 is the driven session, pane 1 the driver, and -c appends the control
// panel as pane 2. magmux is launched FROM workdir so pane 0's claude starts
// there — that is the fast path for transcript discovery (CLAUDE.md: a pane
// whose real cwd differs from magmux's falls back to scanning every project).
// The driver is the one that cds away.
//
// --allowedTools is what makes the run possible non-interactively: `claude -p`
// auto-denies anything that would prompt, and an MCP server added by
// --mcp-config prompts on first use. --disallowedTools is what makes the
// verdict mean something, and it is a deny rule, so it wins over the allow.
//
// --output-format stream-json --verbose is what makes the pane watchable, and
// the pipe into bun is what makes the stream readable; neither grants the
// driver anything, and the deny rule is untouched. `2>&1` so warnings and
// crashes arrive in order through the same filter instead of racing it.
//
// Every path here is single-quoted and none contains a `$`: this whole string
// is handed to `$SHELL -l -c`, which would otherwise expand one (pilot_test.go
// documents the fixture this ate).
const driverCmd =
  `cd ${shq(driverDir)} && claude -p ${shq(DRIVER_GOAL)} ` +
  `--mcp-config ${shq(configPath)} ` +
  `--allowedTools ${shq("mcp__magmux")} ` +
  `--disallowedTools ${shq("Bash,Write,Edit")} ` +
  `--output-format stream-json --verbose 2>&1 ` +
  `| ${shq(process.execPath)} run ${shq(streamPath)}`;

const args = [
  "-c",
  "-w",
  "-e", "claude --dangerously-skip-permissions",
  "-e", driverCmd,
];

console.log(
  `\n  ${C.cyan}Handing the terminal to magmux — bottom left is the driver thinking,` +
    ` right is the control panel.${C.reset}` +
    ` ${C.grey}Verdict prints when it exits.${C.reset}`,
);
await sleep(1200);

const run = new InlineMagmuxRun(BIN, args, workdir);
const sessionStates: string[] = [];      // state transitions of pane 0
const sessionResponses: string[] = [];   // reply text magmux observed on pane 0
const outbound: SnapshotEvent[] = [];    // control dir=out (driver → session)
const notes: SnapshotEvent[] = [];       // control dir=note (start / finish)
const paneOpens: SnapshotEvent[] = [];   // pane_opened broadcasts
const okReplies: SnapshotEvent[] = [];   // reply ok:true, if we are ever sent one
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
          `${at}  ${C.blue}▶ ${String(ev.label ?? "send").padEnd(14)}${C.reset}` +
            `"${truncate(String(ev.text ?? ""), 44)}"`,
        );
      } else if (ev.dir === "note") {
        notes.push(ev);
        timeline.push(
          `${at}  ${C.magenta}• ${String(ev.event).padEnd(14)}${C.reset}` +
            `${truncate(String(ev.client ?? ev.goal ?? ev.summary ?? ""), 44)}`,
        );
      }
      return;
    }

    if (ev.type === "pane_opened") {
      paneOpens.push(ev);
      timeline.push(
        `${at}  ${C.cyan}+ ${"pane_opened".padEnd(14)}${C.reset}` +
          `pane ${ev.pane} ${C.grey}${truncate(String(ev.cmd ?? ""), 34)}${C.reset}`,
      );
      return;
    }

    // Replies are unicast to the connection that asked (the MCP server's own),
    // so this subscriber should never see one — collected anyway because if the
    // unicast property ever regresses, seeing it here is the symptom.
    if (ev.type === "reply") {
      if (ev.ok === true) okReplies.push(ev);
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
        `${state.padEnd(14)}${C.reset}` +
        (resp ? `${C.dim}resp=${C.reset}"${truncate(resp, 40)}"` : `${C.grey}—${C.reset}`),
    );
    return run.hasExited || undefined;
  }, TIMEOUT_MS);

  for (let i = 0; i < HOLD_MS / 100 && !run.hasExited; i++) await sleep(100);
} catch (e) {
  socketErr = e;
} finally {
  await run.stop();
  cleanupTemp();
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
let driverLeft: string[] = [];
try {
  driverLeft = fs.readdirSync(driverDir).sort();
} catch {}

console.log(`\n${C.bold}  Artifacts${C.reset} ${C.grey}${workdir}${C.reset}`);
console.log(`  ${C.grey}driven files: ${produced.join(", ") || "(none)"}${C.reset}`);
console.log(`  ${C.grey}driver files: ${driverLeft.join(", ") || "(none)"}${C.reset}`);
console.log(
  artifact
    ? `  ${artifact.map((l) => `${C.green}${l}${C.reset}`).join(`${C.grey} · ${C.reset}`)}`
    : `  ${C.red}(not created)${C.reset}`,
);

const start = notes.find((n) => n.event === "start");
const mcpClient = String(start?.client ?? "");
const turns = sessionStates.filter((s) => s === "awaiting_input").length;
const panelEntry = (finalResults?.panes as Array<Record<string, unknown>> | undefined)
  ?.find((p) => p.control === true);

/**
 * Labels the MCP server puts on a `send` when the caller supplies none. The pi
 * pilot labels its sends "step N/M", so one of these on an OUT row can only
 * have come through `magmux mcp`.
 */
const MCP_TOOL_LABELS = new Set(["send_and_wait", "send_keys"]);
const mcpLabelled = outbound.filter((ev) => MCP_TOOL_LABELS.has(String(ev.label ?? "")));

const checks: Check[] = [
  {
    // `client` is the one field MCP adds to the pilot protocol, and it is what
    // makes the panel choose the routing layout. Its absence means the driver
    // never got as far as attaching to the host socket.
    label: "magmux mcp attached to its host session and named its client",
    pass: !!start && mcpClient !== "",
    detail: start
      ? `control note event=start client=${mcpClient || "(anonymous)"}`
      : "no start note — the MCP server never attached",
  },
  {
    // >= 1, not >= 2, for case 3's reason: a driver briefed to delegate an
    // outcome will often accomplish a small goal in a single instruction and
    // let the session decompose it. Demanding "multi-step" would punish
    // exactly the behaviour we want.
    label: "driver drove the session through magmux's send path",
    pass: outbound.length >= 1 && turns >= 1,
    detail: `${outbound.length} instruction(s), ${turns} completed turn(s): ${sessionStates.join(" → ") || "none"}`,
  },
  {
    // OUT is the controller's own request; IN is magmux's observation. More
    // completions than instructions would mean the panel is counting turns
    // nobody asked for.
    label: "observed turns never outnumber instructions",
    pass: turns <= outbound.length + 1,
    detail: `${turns} turn(s) vs ${outbound.length} instruction(s)`,
  },
  {
    // Regression guard for transcript discovery: when the controller never
    // locks onto the JSONL, every turn comes back textless and the driver flies
    // blind, burning turns re-asking for output it already has.
    label: "session's own text reached the driver",
    pass: sessionResponses.some((r) => r.trim().length > 0),
    detail: sessionResponses.length
      ? `longest reply ${Math.max(...sessionResponses.map((r) => r.length))} chars`
      : "no response text observed at all",
  },
  {
    // The case-4-specific one. Any of these four is proof the MCP layer was
    // really in the loop rather than something speaking only the legacy
    // fire-and-forget verbs, which case 3 already covers.
    label: "the MCP layer spoke its own protocol, not just legacy verbs",
    pass: paneOpens.length > 0 || okReplies.length > 0 || mcpLabelled.length > 0 || mcpClient !== "",
    detail: `${paneOpens.length} pane_opened · ${okReplies.length} ok reply · ` +
      `${mcpLabelled.length} MCP-labelled send(s) · client=${mcpClient || "(none)"}`,
  },
  {
    label: "control panel reported as a panel, not a stuck session",
    pass: panelEntry?.state === "panel",
    detail: panelEntry ? `state=${panelEntry.state}` : "no control entry in results",
  },
  // The assertions that cannot be satisfied by either agent's self-report.
  {
    label: `${ARTIFACT} exists in the DRIVEN workdir with the right lines`,
    pass: !!artifact && artifact.length === LINES.length && artifact.every((l, i) => l === LINES[i]),
    detail: artifact ? `got [${artifact.join(", ")}] want [${LINES.join(", ")}]` : "file missing",
  },
  {
    // The task named three deliverables; a run that produced only the data file
    // did part of the job while reporting all of it.
    label: "every deliverable the task named exists",
    pass: produced.some((f) => /^readme\.md$/i.test(f)) &&
      produced.some((f) => /\.(py|sh|js|ts|rb|pl)$/i.test(f)),
    detail: produced.length ? produced.join(", ") : "(nothing created)",
  },
  {
    // The whole reason the driver has no shell and its own cwd: if the artifact
    // had appeared over there, the run would prove nothing about magmux at all.
    label: "the driver's own cwd holds no artifact — it could not have done it",
    pass: !driverLeft.some((f) => f === ARTIFACT),
    detail: driverLeft.length ? `driver cwd holds: ${driverLeft.join(", ")}` : "driver cwd empty",
  },
];

const code = report(checks);
console.log(`  ${C.grey}driven workdir kept for inspection: ${workdir}${C.reset}`);
console.log(`  ${C.grey}driver workdir kept for inspection: ${driverDir}${C.reset}\n`);
process.exit(code);
