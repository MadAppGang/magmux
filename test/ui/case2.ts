#!/usr/bin/env bun
/**
 * UI case 2 — watch the real magmux UI drive a grid of Claude Code panes.
 *
 * magmux takes over THIS terminal and renders its grid: you watch each pane
 * work and tint green with its ✓ DONE overlay as it lands. When the run
 * finishes the screen is restored and the verdict is printed. No tmux, no
 * multiplexer, no split — any plain terminal works, which is the point of a
 * visual case.
 *
 * It is also the live repro for issue #3. All three panes run claude from
 * the SAME cwd, so all three transcripts land in the same
 * ~/.claude/projects directory at once. Each pane is told to answer with a
 * different word, and the case asserts every pane reported ITS OWN word —
 * only possible if each controller bound to the right transcript. A
 * controller reading a neighbour's transcript shows up as the wrong word.
 *
 * Deliberately no -w: magmux stays up after the panes finish so the
 * completed grid is on screen to look at (HOLD_MS, default 6s). Press q in
 * the grid to end the hold early.
 */
import { execSync } from "node:child_process";
import path from "node:path";
import {
  C, Check, InlineMagmuxRun, SnapshotEvent, findSocket, header, isLiveSnapshot,
  report, sleep, streamEvents, strippedMarkers, truncate,
} from "./harness.ts";

const REPO = path.resolve(import.meta.dir, "../..");
const BIN = process.env.MAGMUX_BIN ? path.resolve(process.env.MAGMUX_BIN) : path.join(REPO, "magmux");
const HOLD_MS = Number(process.env.HOLD_MS ?? 6000);
const TIMEOUT_MS = 180_000;

/** Distinct answers so a pane reporting a neighbour's transcript is visible. */
const WORDS = ["alpha", "bravo", "charlie"];

header(
  "magmux UI case 2 — watch the grid, and prove per-pane transcript binding",
  "3 claude panes, one shared cwd (issue #3): each must report its own word",
);

console.log(`\n${C.bold}  Setup${C.reset}`);
console.log(`  ${C.grey}cwd    ${C.reset}${REPO}`);
console.log(`  ${C.grey}panes  ${C.reset}${WORDS.map((w, i) => `#${i}→${w}`).join("   ")}`);
console.log(`  ${C.grey}env    ${C.reset}stripping ${strippedMarkers().length} Claude Code marker(s)`);

if (!InlineMagmuxRun.supported()) {
  console.error(
    `\n  ${C.red}This case renders the magmux UI into your terminal, so it needs a TTY.${C.reset}\n` +
      `  ${C.grey}Run it directly (not piped or redirected):  bun run test:ui:case2${C.reset}\n` +
      `  ${C.grey}For a non-visual check that works anywhere:  bun run test:ui:case1${C.reset}\n`,
  );
  process.exit(2);
}

if (!process.env.MAGMUX_BIN) {
  execSync(`go build -o ${JSON.stringify(BIN)} .`, { cwd: REPO, stdio: "inherit" });
  console.log(`  ${C.green}✓${C.reset} built ${path.relative(REPO, BIN)}`);
} else {
  console.log(`  ${C.yellow}!${C.reset} MAGMUX_BIN override ${C.grey}${BIN}${C.reset}`);
}

const args = WORDS.flatMap((w) => [
  "-e",
  `claude --dangerously-skip-permissions 'reply with only the word ${w}'`,
]);

console.log(
  `\n  ${C.cyan}Handing the terminal to magmux — watch the grid.${C.reset}` +
    ` ${C.grey}Verdict prints when it exits.${C.reset}`,
);
await sleep(1200);

const run = new InlineMagmuxRun(BIN, args, REPO);
const seen = new Map<number, SnapshotEvent>();
const timeline: string[] = [];
let socketErr: unknown = null;

run.start();
try {
  const sock = await findSocket(run.startedAt);
  // magmux owns the screen from here: collect silently, report after stop().
  await streamEvents(sock, (ev, t) => {
    if (!isLiveSnapshot(ev)) return;
    const idx = ev.pane as number;
    seen.set(idx, { ...seen.get(idx), ...ev });
    const resp = String(ev.response ?? "").trim();
    timeline.push(
      `  ${C.grey}${t.toFixed(1).padStart(5)}s${C.reset}  ${C.bold}pane ${idx}${C.reset}  ` +
        `${ev.state === "awaiting_input" ? C.green + "●" : ev.state === "working" ? C.yellow + "◐" : C.grey + "○"} ` +
        `${String(ev.state).padEnd(16)}${C.reset}` +
        (resp ? `${C.dim}resp=${C.reset}"${truncate(resp, 22)}"` : `${C.grey}—${C.reset}`),
    );
    const allDone = WORDS.every((_, i) => {
      const s = seen.get(i);
      return s?.state === "awaiting_input" && String(s.response ?? "").trim();
    });
    return allDone || run.hasExited || undefined;
  }, TIMEOUT_MS);

  // Leave the finished grid on screen, unless the user already quit it.
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

const checks: Check[] = [
  {
    label: `all ${WORDS.length} panes attached a controller`,
    pass: WORDS.every((_, i) => seen.get(i)?.controller === "claude-code"),
    detail: `${WORDS.filter((_, i) => seen.get(i)?.controller).length}/${WORDS.length} attached`,
  },
  {
    label: `all ${WORDS.length} panes reached awaiting_input live`,
    pass: WORDS.every((_, i) => seen.get(i)?.state === "awaiting_input"),
    detail: WORDS.map((_, i) => `#${i}=${seen.get(i)?.state ?? "—"}`).join(" "),
  },
  {
    label: "every pane locked onto a transcript",
    pass: WORDS.every((_, i) => !!seen.get(i)?.model),
    detail: WORDS.map((_, i) => `#${i}=${seen.get(i)?.model ? "yes" : "no"}`).join(" "),
  },
  // The issue #3 assertion: with three concurrent sessions in one project
  // directory, a controller that picked the wrong transcript would report a
  // neighbour's word here.
  {
    label: "each pane reported ITS OWN word (no cross-talk)",
    pass: WORDS.every((w, i) => String(seen.get(i)?.response ?? "").toLowerCase().includes(w)),
    detail: WORDS.map((w, i) =>
      `#${i} want=${w} got="${truncate(String(seen.get(i)?.response ?? "—"), 10)}"`).join("  "),
  },
  {
    label: "each pane echoed ITS OWN prompt",
    pass: WORDS.every((w, i) => String(seen.get(i)?.prompt ?? "").toLowerCase().includes(w)),
    detail: WORDS.map((w, i) =>
      `#${i}=${String(seen.get(i)?.prompt ?? "").includes(w) ? "ok" : "MISMATCH"}`).join(" "),
  },
];

process.exit(report(checks));
