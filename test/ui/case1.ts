#!/usr/bin/env bun
/**
 * UI case 1 — a single Claude Code pane reaches awaiting_input LIVE.
 *
 * This is the exact repro from issue #2:
 *   claude --dangerously-skip-permissions "reply with only the word hello"
 *
 * and it is run from this repo, which lives under .claude/worktrees/* — a
 * path containing a dot, which is also the repro for the project-dir
 * encoding bug. Before v0.5.0 this emitted ZERO live snapshot events.
 *
 * What makes this more than a smoke test is the depth check: it is not
 * enough for the pane to reach awaiting_input, because the terminal-idle
 * fallback can produce that on its own with no transcript at all. The case
 * also asserts that transcript discovery actually locked on (model /
 * prompt / response populated), which is the difference between the whole
 * chain working and a hollow green.
 */
import { execSync } from "node:child_process";
import path from "node:path";
import {
  C, Check, MagmuxRun, SnapshotEvent, header, isLiveSnapshot,
  printTimelineRow, report, strippedMarkers, truncate,
} from "./harness.ts";

const PROMPT = "reply with only the word hello";
const REPO = path.resolve(import.meta.dir, "../..");
const BIN = path.join(REPO, "magmux");
const TIMEOUT_MS = 120_000;

header(
  "magmux UI case 1 — live pane state reaches awaiting_input",
  "issue #2 repro, run from a dotted worktree path (issue: project-dir encoding)",
);

console.log(`\n${C.bold}  Setup${C.reset}`);
console.log(`  ${C.grey}cwd    ${C.reset}${REPO}`);
console.log(`  ${C.grey}prompt ${C.reset}"${PROMPT}"`);

// Guard against the hollow pass: if Claude Code session markers leak into
// the child, it disables transcript saving entirely and discovery can never
// lock on, while the pane still reaches awaiting_input via the fallback.
const stripped = strippedMarkers();
console.log(
  `  ${C.grey}env    ${C.reset}stripped ${stripped.length} Claude Code marker(s) ` +
    `${C.grey}${stripped.length ? truncate(stripped.join(" "), 46) : "(none present)"}${C.reset}`,
);

// MAGMUX_BIN lets the case run against an arbitrary build — point it at a
// pre-fix binary to confirm this case actually catches the regression:
//   git archive v0.4.3 | tar -x -C /tmp/base && (cd /tmp/base && go build -o /tmp/mm-old .)
//   MAGMUX_BIN=/tmp/mm-old bun run test:ui:case1   # must FAIL
const override = process.env.MAGMUX_BIN;
const binary = override ? path.resolve(override) : BIN;
console.log(`\n${C.bold}  Build${C.reset}`);
if (override) {
  console.log(`  ${C.yellow}!${C.reset} using MAGMUX_BIN override ${C.grey}${binary}${C.reset}`);
} else {
  execSync(`go build -o ${JSON.stringify(BIN)} .`, { cwd: REPO, stdio: "inherit" });
  console.log(`  ${C.green}✓${C.reset} ${path.relative(REPO, BIN)}`);
}

// -w makes magmux auto-exit once the pane is idle, which also emits the
// final `results` event — letting us assert that the live snapshot and the
// shutdown results can no longer disagree (the heart of issue #2).
const run = new MagmuxRun(
  binary,
  ["-e", `claude --dangerously-skip-permissions '${PROMPT}'`, "-w"],
  REPO,
  "case1",
);

const live: SnapshotEvent[] = [];
let results: SnapshotEvent | null = null;
let connect: SnapshotEvent | null = null;
let paneTail: string[] = [];

console.log(`\n${C.bold}  Live socket timeline${C.reset}`);
try {
  run.start();
  await run.waitForSocket();
  console.log(`  ${C.grey}subscribed ${run.sockPath}${C.reset}\n`);

  await run.readEvents((ev, t) => {
    if (isLiveSnapshot(ev)) {
      live.push(ev);
      printTimelineRow(t, ev);
    } else if (ev.type === "snapshot" && ev.panes) {
      // Connect-time aggregate. Proves the controller is attached even if we
      // subscribed after the first per-pane broadcast.
      connect = ev;
      const p = (ev.panes[0] ?? {}) as SnapshotEvent;
      console.log(
        `  ${C.grey}${t.toFixed(1).padStart(5)}s${C.reset}  ${C.blue}▷ connect${C.reset}` +
          `            ${C.dim}ctrl=${C.reset}${p.controller ?? "—"} ${C.dim}state=${C.reset}${p.state}`,
      );
    } else if (ev.type === "results") {
      results = ev;
      const p = (ev.panes?.[0] ?? {}) as SnapshotEvent;
      console.log(
        `  ${C.grey}${t.toFixed(1).padStart(5)}s${C.reset}  ${C.cyan}▣ results${C.reset}` +
          `            ${C.dim}state=${C.reset}${p.state}`,
      );
      return true; // results is the last thing we need
    }
  }, TIMEOUT_MS);
} finally {
  // Capture the rendered pane BEFORE tearing the tmux server down, or the
  // failure diagnostic below has nothing to show.
  paneTail = run.screenTail(14);
  run.stop();
}

// --- assertions -----------------------------------------------------------

const states = live.map((e) => e.state);
const lockedOn = live.filter((e) => e.model || e.prompt || e.response);
const finalLive = [...live].reverse().find((e) => e.state);
const resultsState = results
  ? ((results.panes?.[0] ?? {}) as SnapshotEvent).state
  : undefined;

const checks: Check[] = [
  {
    label: "controller attached to the pane",
    pass:
      live.some((e) => e.controller === "claude-code") ||
      ((connect?.panes?.[0] as SnapshotEvent | undefined)?.controller === "claude-code"),
    detail:
      live[0]?.controller ??
      (connect?.panes?.[0] as SnapshotEvent | undefined)?.controller ??
      "no controller seen",
  },
  {
    label: "live snapshot reached awaiting_input",
    pass: states.includes("awaiting_input"),
    detail: `states: ${states.join(" → ") || "(none)"}`,
  },
  {
    label: "transcript discovery locked on (not fallback-only)",
    pass: lockedOn.length > 0,
    detail: lockedOn.length
      ? `model=${lockedOn.at(-1)?.model ?? "—"} resp="${truncate(String(lockedOn.at(-1)?.response ?? ""), 18)}"`
      : "model/prompt/response never populated — transcript never found",
  },
  {
    label: "claude's answer captured in the snapshot",
    pass: live.some((e) => String(e.response ?? "").toLowerCase().includes("hello")),
    detail: `response="${truncate(String(live.at(-1)?.response ?? ""), 24)}"`,
  },
  {
    label: "snapshot and results agree (no divergence)",
    pass: !!resultsState && resultsState === finalLive?.state,
    detail: `snapshot=${finalLive?.state ?? "—"} results=${resultsState ?? "—"}`,
  },
];

const code = report(checks);
if (code !== 0) {
  // Show what the pane actually rendered — usually says exactly why (auth
  // prompt, trust dialog, transcript-saving warning, crash).
  console.log(`${C.bold}  Pane output (tail)${C.reset}`);
  for (const line of paneTail) {
    console.log(`  ${C.grey}│${C.reset} ${line.slice(0, 100)}`);
  }
  console.log();
}
process.exit(code);
