/**
 * Shared harness for magmux UI test cases.
 *
 * These cases drive a REAL magmux binary with REAL Claude Code panes and
 * watch the IPC socket, so they validate the whole chain the way a
 * subscriber (claudish, madbench) experiences it.
 */
import { execFileSync } from "node:child_process";
import net from "node:net";
import fs from "node:fs";
import path from "node:path";

export const C = {
  reset: "\x1b[0m",
  dim: "\x1b[2m",
  bold: "\x1b[1m",
  red: "\x1b[31m",
  green: "\x1b[32m",
  yellow: "\x1b[33m",
  blue: "\x1b[34m",
  magenta: "\x1b[35m",
  cyan: "\x1b[36m",
  grey: "\x1b[90m",
};

/**
 * Claude Code refuses to write a transcript when it detects it was spawned
 * from inside another Claude Code session ("Transcript saving is off —
 * inherited CLAUDE_CODE_CHILD_SESSION marker"). Any harness that forgets to
 * strip these markers still sees the pane reach awaiting_input via the
 * terminal-idle fallback and looks like it passed, while never exercising
 * transcript discovery at all. That false pass is the single easiest way to
 * get a hollow green, so stripping is not optional.
 */
export function cleanEnv(): NodeJS.ProcessEnv {
  const env: NodeJS.ProcessEnv = {};
  for (const [k, v] of Object.entries(process.env)) {
    if (k === "CLAUDECODE" || k.startsWith("CLAUDE_")) continue;
    env[k] = v;
  }
  env.TERM = "screen-256color";
  return env;
}

export function strippedMarkers(): string[] {
  return Object.keys(process.env)
    .filter((k) => k === "CLAUDECODE" || k.startsWith("CLAUDE_"))
    .sort();
}

export interface SnapshotEvent {
  type: string;
  pane?: number;
  panes?: unknown[];
  state?: string;
  controller?: string;
  model?: string;
  project?: string;
  prompt?: string;
  response?: string;
  tool?: string;
  [k: string]: unknown;
}

/** A magmux process running under a pseudo-terminal, plus its IPC socket. */
export class MagmuxRun {
  sockPath = "";
  private startedAt = 0;

  constructor(
    readonly binary: string,
    readonly args: string[],
    readonly cwd: string,
    sessionName = "case",
  ) {
    this.session = sessionName;
  }

  /**
   * magmux needs a real TTY, and it needs stdin to STAY OPEN — with stdin at
   * EOF, claude quits immediately instead of idling at its prompt, which is
   * the state these cases are about. Bun has no pty binding and script(1)
   * gives us one or the other but not both (a /dev/null stdin EOFs the pane;
   * a pipe stdin makes script itself fail).
   *
   * tmux solves both cleanly, and as a bonus `capture-pane` gives us a
   * properly rendered screen for diagnostics instead of a raw escape stream.
   * We use a dedicated server (-L) so the user's own sessions are untouched.
   */
  private readonly server = "magmux-ui";
  private readonly session: string;

  start() {
    this.startedAt = Date.now();
    const cmd = [this.binary, ...this.args]
      .map((a) => (/[\s"']/.test(a) ? `'${a.replace(/'/g, `'\\''`)}'` : a))
      .join(" ");

    // Start from a clean server so it inherits our stripped environment
    // rather than whatever a stale server was started with.
    this.tmux(["kill-server"], true);
    this.tmux([
      "new-session", "-d",
      "-s", this.session,
      "-c", this.cwd,
      "-x", "200", "-y", "50",
      cmd,
    ]);
  }

  private tmux(args: string[], ignoreError = false): string {
    try {
      return execFileSync("tmux", ["-L", this.server, ...args], {
        env: cleanEnv(),
        encoding: "utf8",
        stdio: ["ignore", "pipe", ignoreError ? "ignore" : "pipe"],
      });
    } catch (e) {
      if (ignoreError) return "";
      throw e;
    }
  }

  /** Last `n` non-blank rendered lines of the pane — what it actually showed. */
  screenTail(n = 12): string[] {
    const out = this.tmux(["capture-pane", "-p", "-t", this.session], true);
    return out.split("\n").map((l) => l.trimEnd()).filter((l) => l.trim()).slice(-n);
  }

  /** magmux binds /tmp/magmux-<pid>.sock; script(1) hides that pid from us,
   *  so find the socket that appeared after we started. */
  async waitForSocket(timeoutMs = 15000): Promise<string> {
    const deadline = Date.now() + timeoutMs;
    while (Date.now() < deadline) {
      let entries: string[] = [];
      try {
        entries = fs.readdirSync("/tmp").filter((f) => /^magmux-\d+\.sock$/.test(f));
      } catch {}
      for (const e of entries) {
        const full = path.join("/tmp", e);
        try {
          if (fs.statSync(full).ctimeMs >= this.startedAt - 500) {
            this.sockPath = full;
            return full;
          }
        } catch {}
      }
      await sleep(50);
    }
    throw new Error("magmux socket never appeared");
  }

  /** Stream line-delimited JSON events until `onEvent` returns true or we time out. */
  async readEvents(
    onEvent: (ev: SnapshotEvent, tSec: number) => boolean | void,
    timeoutMs: number,
  ): Promise<void> {
    const t0 = Date.now();
    await new Promise<void>((resolve) => {
      const conn = net.createConnection(this.sockPath);
      let buf = "";
      let done = false;
      const finish = () => {
        if (done) return;
        done = true;
        try { conn.destroy(); } catch {}
        clearTimeout(timer);
        resolve();
      };
      const timer = setTimeout(finish, timeoutMs);
      conn.on("data", (chunk) => {
        buf += chunk.toString();
        let nl: number;
        while ((nl = buf.indexOf("\n")) >= 0) {
          const line = buf.slice(0, nl).trim();
          buf = buf.slice(nl + 1);
          if (!line) continue;
          let ev: SnapshotEvent;
          try { ev = JSON.parse(line); } catch { continue; }
          if (onEvent(ev, (Date.now() - t0) / 1000) === true) return finish();
        }
      });
      conn.on("end", finish);
      conn.on("error", finish);
    });
  }

  stop() {
    this.tmux(["kill-server"], true);
  }
}

export const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

/** Per-pane live snapshot (has `pane`, not the connect-time `panes` aggregate). */
export function isLiveSnapshot(ev: SnapshotEvent): boolean {
  return ev.type === "snapshot" && ev.pane !== undefined && ev.panes === undefined;
}

// --- reporting ------------------------------------------------------------

export interface Check {
  label: string;
  pass: boolean;
  detail: string;
}

export function header(title: string, subtitle: string) {
  const line = "─".repeat(74);
  console.log(`${C.cyan}┌${line}┐${C.reset}`);
  console.log(`${C.cyan}│${C.reset} ${C.bold}${title.padEnd(72)}${C.reset}${C.cyan}│${C.reset}`);
  console.log(`${C.cyan}│${C.reset} ${C.grey}${subtitle.padEnd(72)}${C.reset}${C.cyan}│${C.reset}`);
  console.log(`${C.cyan}└${line}┘${C.reset}`);
}

const STATE_STYLE: Record<string, string> = {
  starting: C.grey,
  working: C.yellow,
  awaiting_input: C.green,
  awaiting_permission: C.magenta,
  error: C.red,
  gone: C.grey,
};

export function printTimelineRow(tSec: number, ev: SnapshotEvent) {
  const st = ev.state ?? "?";
  const col = STATE_STYLE[st] ?? C.reset;
  const dot = st === "awaiting_input" ? "●" : st === "working" ? "◐" : "○";
  const bits: string[] = [];
  if (ev.model) bits.push(`${C.dim}model=${C.reset}${ev.model}`);
  if (ev.prompt) bits.push(`${C.dim}prompt=${C.reset}"${truncate(String(ev.prompt), 28)}"`);
  if (ev.response) bits.push(`${C.dim}resp=${C.reset}"${truncate(String(ev.response), 20)}"`);
  if (ev.tool) bits.push(`${C.dim}tool=${C.reset}${ev.tool}`);
  console.log(
    `  ${C.grey}${tSec.toFixed(1).padStart(5)}s${C.reset}  ${col}${dot} ${st.padEnd(18)}${C.reset}` +
      (bits.length ? bits.join("  ") : `${C.grey}—${C.reset}`),
  );
}

export function truncate(s: string, n: number): string {
  const flat = s.replace(/\s+/g, " ").trim();
  return flat.length > n ? flat.slice(0, n - 1) + "…" : flat;
}

export function report(checks: Check[]): number {
  console.log(`\n${C.bold}  Assertions${C.reset}`);
  for (const c of checks) {
    const mark = c.pass ? `${C.green}✓${C.reset}` : `${C.red}✗${C.reset}`;
    const lbl = c.pass ? c.label : `${C.red}${c.label}${C.reset}`;
    console.log(`  ${mark} ${lbl.padEnd(52)} ${C.grey}${c.detail}${C.reset}`);
  }
  const failed = checks.filter((c) => !c.pass);
  console.log();
  if (failed.length === 0) {
    console.log(`  ${C.green}${C.bold}PASS${C.reset} ${C.grey}— ${checks.length}/${checks.length} checks${C.reset}\n`);
    return 0;
  }
  console.log(`  ${C.red}${C.bold}FAIL${C.reset} ${C.grey}— ${failed.length}/${checks.length} checks failed${C.reset}\n`);
  return 1;
}
