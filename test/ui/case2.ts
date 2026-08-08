#!/usr/bin/env bun
/**
 * UI case 2 — watch the real magmux UI drive a grid of Claude Code panes.
 *
 * Unlike case 1, which reports from the socket, this case puts the actual
 * magmux interface in a pane beside you: you watch the grid render, each
 * pane work, and each one tint green with its ✓ DONE overlay as it lands.
 *
 * It is also the live repro for issue #3. All three panes run claude from
 * the SAME cwd, so all three transcripts land in the same
 * ~/.claude/projects directory at the same time. Each pane is given a
 * different one-word answer to produce, and the case asserts every pane
 * reported ITS OWN word — which is only possible if each controller bound
 * to the right transcript. Cross-talk between concurrent sessions would
 * show up here as a pane reporting a neighbour's word.
 *
 * Deliberately no -w: magmux stays up after the panes finish so the
 * completed grid remains on screen to look at.
 */
import { execSync } from "node:child_process";
import path from "node:path";
import {
  C, Check, SnapshotEvent, envStripPrefix, findSocket, header, isLiveSnapshot,
  killPane, report, sleep, splitVisiblePane, streamEvents, strippedMarkers,
  truncate,
} from "./harness.ts";

const REPO = path.resolve(import.meta.dir, "../..");
const BIN = process.env.MAGMUX_BIN ? path.resolve(process.env.MAGMUX_BIN) : path.join(REPO, "magmux");
const HOLD_MS = Number(process.env.HOLD_MS ?? 8000);
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

if (!process.env.MAGMUX_BIN) {
  execSync(`go build -o ${JSON.stringify(BIN)} .`, { cwd: REPO, stdio: "inherit" });
  console.log(`  ${C.green}✓${C.reset} built ${path.relative(REPO, BIN)}`);
} else {
  console.log(`  ${C.yellow}!${C.reset} MAGMUX_BIN override ${C.grey}${BIN}${C.reset}`);
}

const paneArgs = WORDS.map(
  (w) => `-e "claude --dangerously-skip-permissions 'reply with only the word ${w}'"`,
).join(" ");
const cmd = `${envStripPrefix()} ${JSON.stringify(BIN)} ${paneArgs}`;

const startedAt = Date.now();
let uiPane = "";
const seen = new Map<number, SnapshotEvent>();

console.log(`\n${C.bold}  Live grid${C.reset}  ${C.grey}(magmux UI is the pane to the right)${C.reset}\n`);

try {
  uiPane = splitVisiblePane(cmd, REPO);
  const sock = await findSocket(startedAt);
  console.log(`  ${C.grey}pane ${uiPane}   socket ${sock}${C.reset}\n`);

  await streamEvents(sock, (ev, t) => {
    if (!isLiveSnapshot(ev)) return;
    const idx = ev.pane as number;
    const prev = seen.get(idx);
    seen.set(idx, { ...prev, ...ev });
    const st = ev.state ?? "?";
    const col = st === "awaiting_input" ? C.green : st === "working" ? C.yellow : C.grey;
    const dot = st === "awaiting_input" ? "●" : st === "working" ? "◐" : "○";
    const resp = String(ev.response ?? "").trim();
    console.log(
      `  ${C.grey}${t.toFixed(1).padStart(5)}s${C.reset}  ${C.bold}pane ${idx}${C.reset}  ` +
        `${col}${dot} ${st.padEnd(16)}${C.reset}` +
        (resp ? `${C.dim}resp=${C.reset}"${truncate(resp, 22)}"` : `${C.grey}—${C.reset}`),
    );
    // Done once every pane has settled with an answer.
    const done = WORDS.every((_, i) => {
      const s = seen.get(i);
      return s?.state === "awaiting_input" && String(s.response ?? "").trim();
    });
    return done || undefined;
  }, TIMEOUT_MS);

  console.log(`\n  ${C.grey}holding the finished grid on screen for ${HOLD_MS / 1000}s…${C.reset}`);
  await sleep(HOLD_MS);
} finally {
  if (uiPane) killPane(uiPane);
}

// --- assertions -----------------------------------------------------------

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
    pass: WORDS.every((w, i) =>
      String(seen.get(i)?.response ?? "").toLowerCase().includes(w)),
    detail: WORDS.map((w, i) =>
      `#${i} want=${w} got="${truncate(String(seen.get(i)?.response ?? "—"), 10)}"`).join("  "),
  },
  {
    label: "each pane echoed ITS OWN prompt",
    pass: WORDS.every((w, i) =>
      String(seen.get(i)?.prompt ?? "").toLowerCase().includes(w)),
    detail: WORDS.map((w, i) =>
      `#${i}=${String(seen.get(i)?.prompt ?? "").includes(w) ? "ok" : "MISMATCH"}`).join(" "),
  },
];

process.exit(report(checks));
