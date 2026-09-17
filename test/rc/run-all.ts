#!/usr/bin/env bun
/**
 * Run all six remote-control validation cases and print the matrix.
 *
 * Each case is its own PROCESS. That is not tidiness: every one of them starts
 * a real magmux, binds a real port and, in two of them, drives a real database
 * emulator, and a crash or a leaked listener in one must not be able to take
 * the others with it or silently change what they measure.
 *
 * Each case prints a `__RC__ <n> PASS|FAIL|SKIP <name> — <detail>` line, which
 * is what this reads. The rest of a case's output is for a human.
 *
 *   bun test/rc/run-all.ts            # 1, 2, 4, 5, 6; 3 skips
 *   MAGMUX_FIREBASE_EMULATOR=1 …      # all six
 *
 * Exit status is the number of FAILED criteria, so `task test:rc` fails the
 * build with a count rather than a boolean.
 */
import { spawnSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { C, REPO, buildMagmux, header } from "./harness.ts";

const CASES: { n: number; file: string; what: string }[] = [
  { n: 1, file: "case1-http.ts", what: "HTTP: token, 401, view-token 403" },
  { n: 2, file: "case2-ws.ts", what: "WebSocket: watch, type, frame within 1s" },
  { n: 3, file: "case3-firebase.ts", what: "Firebase: mirror, signed command, unsigned refused" },
  { n: 4, file: "case4-mcp.ts", what: "MCP: pane resources, subscribe → updated" },
  { n: 5, file: "case5-plugin.ts", what: "Demo plugin over every transport" },
  { n: 6, file: "case6-slow-client.ts", what: "Slow client: stall contained" },
];

header(
  "magmux remote control — the six validation criteria",
  process.env.MAGMUX_FIREBASE_EMULATOR === "1"
    ? "all six, including the Firebase emulator"
    : "set MAGMUX_FIREBASE_EMULATOR=1 to include criterion 3 (needs firebase-tools and a JDK 21+)",
);

// Built ONCE here so the cases do not each rebuild, and so a compile error is
// one message at the top rather than six identical ones interleaved with
// results.
console.log(`\n${C.bold}  Build${C.reset}`);
const bin = buildMagmux();
console.log(`  ${C.green}✓${C.reset} ${bin}`);

interface Outcome {
  n: number;
  what: string;
  verdict: "PASS" | "FAIL" | "SKIP";
  detail: string;
  ms: number;
}

const only = process.argv.slice(2).filter((a) => /^\d+$/.test(a)).map(Number);
const outcomes: Outcome[] = [];

for (const c of CASES) {
  if (only.length && !only.includes(c.n)) continue;
  console.log(`\n${C.cyan}${"─".repeat(76)}${C.reset}`);
  const started = Date.now();
  const res = spawnSync("bun", [path.join("test/rc", c.file)], {
    cwd: REPO,
    stdio: ["ignore", "pipe", "inherit"],
    // MAGMUX_BIN so each case uses the binary built above rather than rebuilding.
    env: { ...process.env, MAGMUX_BIN: bin },
    encoding: "utf8",
    maxBuffer: 64 * 1024 * 1024,
  });
  const out = res.stdout ?? "";
  process.stdout.write(out);

  const line = out.split("\n").reverse().find((l) => l.startsWith("__RC__ "));
  const m = line ? /^__RC__ (\d+) (PASS|FAIL|SKIP) (.*?) — (.*)$/.exec(line) : null;
  outcomes.push({
    n: c.n,
    what: m ? m[3] : c.what,
    // No marker line means the case died before reporting, which is a failure
    // however it exited — a case that cannot say what it found has found
    // nothing.
    verdict: m ? (m[2] as Outcome["verdict"]) : "FAIL",
    detail: m ? m[4] : `the case produced no result line (exit ${res.status}${res.error ? `, ${res.error}` : ""})`,
    ms: Date.now() - started,
  });
}

// ── the matrix ──────────────────────────────────────────────────────────────

const mark = (v: Outcome["verdict"]) =>
  v === "PASS" ? `${C.green}PASS${C.reset}` : v === "SKIP" ? `${C.yellow}SKIP${C.reset}` : `${C.red}FAIL${C.reset}`;

console.log(`\n${C.cyan}${"═".repeat(76)}${C.reset}`);
console.log(`${C.bold}  Validation criteria${C.reset}\n`);
for (const o of outcomes) {
  console.log(`  ${mark(o.verdict)}  ${C.bold}${o.n}${C.reset}  ${o.what.padEnd(56)} ${C.grey}${(o.ms / 1000).toFixed(1)}s${C.reset}`);
  console.log(`        ${C.grey}${o.detail}${C.reset}`);
}

const failed = outcomes.filter((o) => o.verdict === "FAIL");
const skipped = outcomes.filter((o) => o.verdict === "SKIP");
console.log();
console.log(
  failed.length
    ? `  ${C.red}${C.bold}${failed.length} of ${outcomes.length} criteria FAILED${C.reset}`
    : `  ${C.green}${C.bold}${outcomes.length - skipped.length} of ${outcomes.length} criteria PASS${C.reset}` +
        (skipped.length ? `${C.grey}, ${skipped.length} skipped${C.reset}` : ""),
);
console.log();

// A machine-readable copy beside the human one, for the session's evidence.
if (process.env.MAGMUX_RC_RESULT) {
  const md = [
    "# Remote control — the six validation criteria",
    "",
    `Run: ${new Date().toISOString()}`,
    `Binary: ${bin}`,
    `Firebase emulator: ${process.env.MAGMUX_FIREBASE_EMULATOR === "1" ? "enabled" : "not enabled"}`,
    "",
    ...outcomes.map((o) => `- **${o.verdict}** — criterion ${o.n}: ${o.what} — ${o.detail} (${(o.ms / 1000).toFixed(1)}s)`),
    "",
  ].join("\n");
  fs.mkdirSync(path.dirname(process.env.MAGMUX_RC_RESULT), { recursive: true });
  fs.writeFileSync(process.env.MAGMUX_RC_RESULT, md);
  console.log(`  ${C.grey}wrote ${process.env.MAGMUX_RC_RESULT}${C.reset}\n`);
}

process.exit(failed.length);
