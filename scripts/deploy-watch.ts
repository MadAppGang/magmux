#!/usr/bin/env bun
// deploy-watch.ts — TUI dashboard for monitoring GitHub Actions deployments
// Zero npm dependencies. Uses Bun APIs + raw ANSI escape codes.

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

interface WorkflowRun {
  id: number;
  name: string;
  display_title: string;
  status: string; // queued | in_progress | completed | waiting
  conclusion: string | null; // success | failure | cancelled | skipped | null
  event: string;
  head_branch: string;
  run_number: number;
  html_url: string;
  created_at: string;
  updated_at: string;
  run_started_at: string;
}

interface Job {
  id: number;
  name: string;
  status: string;
  conclusion: string | null;
  started_at: string | null;
  completed_at: string | null;
  steps: Step[];
  runner_name: string;
  labels: string[];
}

interface Step {
  name: string;
  status: string; // queued | in_progress | completed
  conclusion: string | null; // success | failure | cancelled | skipped | null
  number: number;
  started_at: string | null;
  completed_at: string | null;
}

interface DashboardState {
  run: WorkflowRun | null;
  jobs: Job[];
  logs: string[];
  error: string | null;
  lastFetch: number;
  showLogs: boolean;
  selectedJobIdx: number;
  scrollOffset: number;
  spinnerFrame: number;
  termWidth: number;
  termHeight: number;
}

// ---------------------------------------------------------------------------
// ANSI helpers
// ---------------------------------------------------------------------------

const ESC = "\x1b";
const CSI = `${ESC}[`;

const ansi = {
  // Screen
  altScreenOn: `${CSI}?1049h`,
  altScreenOff: `${CSI}?1049l`,
  cursorHide: `${CSI}?25l`,
  cursorShow: `${CSI}?25h`,
  clear: `${CSI}2J${CSI}H`,
  moveTo: (row: number, col: number) => `${CSI}${row};${col}H`,

  // Style
  reset: `${CSI}0m`,
  bold: `${CSI}1m`,
  dim: `${CSI}2m`,
  italic: `${CSI}3m`,
  underline: `${CSI}4m`,

  // Foreground colors (256-color)
  fg: (n: number) => `${CSI}38;5;${n}m`,
  bg: (n: number) => `${CSI}48;5;${n}m`,

  // Named colors
  fgWhite: `${CSI}37m`,
  fgGreen: `${CSI}32m`,
  fgYellow: `${CSI}33m`,
  fgRed: `${CSI}31m`,
  fgCyan: `${CSI}36m`,
  fgGray: `${CSI}38;5;8m`,
  fgDimWhite: `${CSI}38;5;250m`,
  bgHighlight: `${CSI}48;5;236m`,
};

// ---------------------------------------------------------------------------
// Spinner animation frames
// ---------------------------------------------------------------------------

const SPINNER_FRAMES = ["⟳", "◐", "◑", "◒", "◓"];

// ---------------------------------------------------------------------------
// Status icons and colors
// ---------------------------------------------------------------------------

function statusIcon(status: string, conclusion: string | null, frame: number): string {
  if (status === "completed") {
    switch (conclusion) {
      case "success":
        return `${ansi.fgGreen}✓${ansi.reset}`;
      case "failure":
        return `${ansi.bold}${ansi.fgRed}✗${ansi.reset}`;
      case "cancelled":
        return `${ansi.dim}${ansi.fgYellow}⊘${ansi.reset}`;
      case "skipped":
        return `${ansi.fgGray}⊙${ansi.reset}`;
      default:
        return `${ansi.fgGreen}✓${ansi.reset}`;
    }
  }
  if (status === "in_progress") {
    return `${ansi.bold}${ansi.fgYellow}${SPINNER_FRAMES[frame % SPINNER_FRAMES.length]}${ansi.reset}`;
  }
  if (status === "queued" || status === "waiting" || status === "pending") {
    return `${ansi.fgGray}○${ansi.reset}`;
  }
  return `${ansi.fgGray}○${ansi.reset}`;
}

function statusColor(status: string, conclusion: string | null): string {
  if (status === "completed") {
    switch (conclusion) {
      case "success":
        return ansi.fgGreen;
      case "failure":
        return `${ansi.bold}${ansi.fgRed}`;
      case "cancelled":
        return `${ansi.dim}${ansi.fgYellow}`;
      case "skipped":
        return ansi.fgGray;
      default:
        return ansi.fgGreen;
    }
  }
  if (status === "in_progress") {
    return `${ansi.bold}${ansi.fgYellow}`;
  }
  return ansi.fgGray;
}

function statusLabel(status: string, conclusion: string | null): string {
  if (status === "completed") return conclusion ?? "completed";
  return status;
}

// ---------------------------------------------------------------------------
// Time formatting
// ---------------------------------------------------------------------------

function formatDuration(ms: number): string {
  const totalSeconds = Math.floor(ms / 1000);
  if (totalSeconds < 60) return `${totalSeconds}s`;
  const minutes = Math.floor(totalSeconds / 60);
  const seconds = totalSeconds % 60;
  if (minutes < 60) return `${minutes}m ${seconds}s`;
  const hours = Math.floor(minutes / 60);
  const remainMins = minutes % 60;
  return `${hours}h ${remainMins}m`;
}

function elapsed(startTime: string): string {
  const start = new Date(startTime).getTime();
  const now = Date.now();
  return formatDuration(now - start);
}

function jobDuration(job: Job): string {
  if (!job.started_at) return "-";
  const start = new Date(job.started_at).getTime();
  const end = job.completed_at ? new Date(job.completed_at).getTime() : Date.now();
  return formatDuration(end - start);
}

function stepDuration(step: Step): string {
  if (!step.started_at) return "-";
  const start = new Date(step.started_at).getTime();
  const end = step.completed_at ? new Date(step.completed_at).getTime() : Date.now();
  return formatDuration(end - start);
}

// ---------------------------------------------------------------------------
// String utilities
// ---------------------------------------------------------------------------

/** Visible length of a string (strips ANSI escape codes) */
function visibleLength(s: string): number {
  // eslint-disable-next-line no-control-regex
  return s.replace(/\x1b\[[0-9;]*m/g, "").length;
}

/** Pad string to width (accounting for ANSI codes) */
function padEnd(s: string, width: number): string {
  const vl = visibleLength(s);
  if (vl >= width) return s;
  return s + " ".repeat(width - vl);
}

/** Truncate visible text to maxLen, adding "..." if needed */
function truncate(s: string, maxLen: number): string {
  if (maxLen <= 0) return "";
  if (s.length <= maxLen) return s;
  if (maxLen <= 3) return s.slice(0, maxLen);
  return s.slice(0, maxLen - 3) + "...";
}

/** Center text within width */
function center(s: string, width: number): string {
  const vl = visibleLength(s);
  if (vl >= width) return s;
  const leftPad = Math.floor((width - vl) / 2);
  const rightPad = width - vl - leftPad;
  return " ".repeat(leftPad) + s + " ".repeat(rightPad);
}

// ---------------------------------------------------------------------------
// Box drawing
// ---------------------------------------------------------------------------

const box = {
  tl: "┌",
  tr: "┐",
  bl: "└",
  br: "┘",
  h: "─",
  v: "│",
  lt: "├",
  rt: "┤",
};

function drawTopBorder(leftTitle: string, rightTitle: string, width: number): string {
  const inner = width - 2; // subtract corners
  const left = `${box.h} ${leftTitle} `;
  const right = ` ${rightTitle} ${box.h}`;
  const fillLen = inner - left.length - right.length;
  const fill = fillLen > 0 ? box.h.repeat(fillLen) : "";
  return `${ansi.fgGray}${box.tl}${left}${fill}${right}${box.tr}${ansi.reset}`;
}

function drawSectionBorder(title: string, width: number): string {
  const inner = width - 2;
  const label = `${box.h} ${title} `;
  const fillLen = inner - label.length;
  const fill = fillLen > 0 ? box.h.repeat(fillLen) : "";
  return `${ansi.fgGray}${box.lt}${label}${fill}${box.rt}${ansi.reset}`;
}

function drawBottomBorder(width: number): string {
  return `${ansi.fgGray}${box.bl}${box.h.repeat(width - 2)}${box.br}${ansi.reset}`;
}

function drawRow(content: string, width: number): string {
  const inner = width - 4; // "│ " ... " │"
  const padded = padEnd(content, inner);
  // Trim if over
  const vis = visibleLength(padded);
  let final = padded;
  if (vis > inner) {
    // crude truncation — just cut the raw string (imperfect with ANSI but acceptable)
    final = padded.slice(0, inner) + ansi.reset;
  }
  return `${ansi.fgGray}${box.v}${ansi.reset} ${final} ${ansi.fgGray}${box.v}${ansi.reset}`;
}

function drawEmptyRow(width: number): string {
  return drawRow("", width);
}

function drawFooterRow(items: string[], width: number): string {
  const inner = width - 4;
  const joined = items.join(`${ansi.fgGray}  │  ${ansi.reset}`);
  return drawRow(`${ansi.dim}${joined}${ansi.reset}`, width);
}

// ---------------------------------------------------------------------------
// Data fetching via `gh` CLI
// ---------------------------------------------------------------------------

async function ghApi(endpoint: string): Promise<{ ok: boolean; data: any; error?: string }> {
  try {
    const proc = Bun.spawn(["gh", "api", endpoint, "--cache=0s"], {
      stdout: "pipe",
      stderr: "pipe",
    });
    const [stdout, stderr] = await Promise.all([
      new Response(proc.stdout).text(),
      new Response(proc.stderr).text(),
    ]);
    const exitCode = await proc.exited;
    if (exitCode !== 0) {
      return { ok: false, data: null, error: stderr.trim() || `gh exited with code ${exitCode}` };
    }
    return { ok: true, data: JSON.parse(stdout) };
  } catch (e: any) {
    return { ok: false, data: null, error: e.message ?? String(e) };
  }
}

async function fetchLatestRun(repo: string): Promise<{ ok: boolean; run?: WorkflowRun; error?: string }> {
  // Try to find an in-progress or queued run first
  const inProgress = await ghApi(
    `repos/${repo}/actions/runs?status=in_progress&per_page=1`
  );
  if (inProgress.ok && inProgress.data.workflow_runs?.length > 0) {
    return { ok: true, run: inProgress.data.workflow_runs[0] };
  }

  const queued = await ghApi(
    `repos/${repo}/actions/runs?status=queued&per_page=1`
  );
  if (queued.ok && queued.data.workflow_runs?.length > 0) {
    return { ok: true, run: queued.data.workflow_runs[0] };
  }

  // Fall back to latest run of any status
  const latest = await ghApi(`repos/${repo}/actions/runs?per_page=1`);
  if (!latest.ok) return { ok: false, error: latest.error };
  if (!latest.data.workflow_runs?.length) {
    return { ok: false, error: "No workflow runs found" };
  }
  return { ok: true, run: latest.data.workflow_runs[0] };
}

async function fetchRun(repo: string, runId: number): Promise<{ ok: boolean; run?: WorkflowRun; error?: string }> {
  const result = await ghApi(`repos/${repo}/actions/runs/${runId}`);
  if (!result.ok) return { ok: false, error: result.error };
  return { ok: true, run: result.data };
}

async function fetchJobs(repo: string, runId: number): Promise<{ ok: boolean; jobs?: Job[]; error?: string }> {
  const result = await ghApi(`repos/${repo}/actions/runs/${runId}/jobs`);
  if (!result.ok) return { ok: false, error: result.error };
  return { ok: true, jobs: result.data.jobs ?? [] };
}

async function fetchJobLogs(repo: string, jobId: number): Promise<string[]> {
  try {
    const proc = Bun.spawn(
      ["gh", "api", `repos/${repo}/actions/jobs/${jobId}/logs`, "--cache=0s"],
      { stdout: "pipe", stderr: "pipe" }
    );
    const stdout = await new Response(proc.stdout).text();
    const exitCode = await proc.exited;
    if (exitCode !== 0) return [];
    // Return last 15 non-empty lines
    const lines = stdout
      .split("\n")
      .map((l) => l.replace(/^\d{4}-\d{2}-\d{2}T[\d:.]+Z\s*/, "").trimEnd())
      .filter((l) => l.length > 0);
    return lines.slice(-15);
  } catch {
    return [];
  }
}

// ---------------------------------------------------------------------------
// Progress bar
// ---------------------------------------------------------------------------

function progressBar(completed: number, total: number, width: number): string {
  if (total === 0) return "";
  const pct = Math.min(1, completed / total);
  const barWidth = Math.max(10, width - 8); // "[" + "]" + " XX%"
  const filled = Math.round(pct * barWidth);
  const empty = barWidth - filled;
  const pctStr = `${Math.round(pct * 100)}%`;
  return (
    `${ansi.fgGray}[${ansi.reset}` +
    `${ansi.fgGreen}${"█".repeat(filled)}${ansi.reset}` +
    `${ansi.fgGray}${"░".repeat(empty)}${ansi.reset}` +
    `${ansi.fgGray}]${ansi.reset} ${ansi.fgCyan}${pctStr}${ansi.reset}`
  );
}

// ---------------------------------------------------------------------------
// Render engine
// ---------------------------------------------------------------------------

function render(state: DashboardState): string {
  const w = state.termWidth;
  const h = state.termHeight;
  const lines: string[] = [];

  if (!state.run) {
    // No run state — show error or loading
    lines.push(drawTopBorder("Deploy Watch", "", w));
    lines.push(drawEmptyRow(w));
    if (state.error) {
      lines.push(drawRow(`${ansi.fgRed}${state.error}${ansi.reset}`, w));
    } else {
      lines.push(drawRow(`${ansi.dim}Loading...${ansi.reset}`, w));
    }
    lines.push(drawEmptyRow(w));
    lines.push(drawBottomBorder(w));
    return lines.join("\n");
  }

  const run = state.run;
  const jobs = state.jobs;
  const frame = state.spinnerFrame;

  // --- Header ---
  lines.push(drawTopBorder("Deploy Watch", run.html_url ? run.html_url.split("/").slice(3, 5).join("/") : "", w));

  // Run title line
  const runIcon = statusIcon(run.status, run.conclusion, frame);
  const runTitle = `${runIcon} ${ansi.bold}${truncate(run.display_title || run.name, w - 40)}${ansi.reset}`;
  const runIdStr = `${ansi.fgGray}run #${run.run_number}${ansi.reset}`;
  const titleInner = w - 4;
  const titleLeftVis = visibleLength(runTitle);
  const titleRightVis = visibleLength(runIdStr);
  const titleGap = titleInner - titleLeftVis - titleRightVis;
  if (titleGap > 0) {
    lines.push(drawRow(runTitle + " ".repeat(titleGap) + runIdStr, w));
  } else {
    lines.push(drawRow(runTitle, w));
  }

  // Trigger line
  const trigger = `Triggered by: ${run.event}${run.head_branch ? ` (${run.head_branch})` : ""}`;
  const elapsedStr = `${ansi.fgCyan}${elapsed(run.run_started_at || run.created_at)} elapsed${ansi.reset}`;
  const trigInner = w - 4;
  const trigLeftVis = trigger.length;
  const trigRightVis = visibleLength(elapsedStr);
  const trigGap = trigInner - trigLeftVis - trigRightVis;
  if (trigGap > 0) {
    lines.push(drawRow(`${ansi.dim}${trigger}${ansi.reset}` + " ".repeat(trigGap) + elapsedStr, w));
  } else {
    lines.push(drawRow(`${ansi.dim}${trigger}${ansi.reset}`, w));
  }

  // --- Jobs section ---
  lines.push(drawSectionBorder("Jobs", w));

  // Find the active job (first in_progress, or first queued, or last)
  let activeJobIdx = jobs.findIndex((j) => j.status === "in_progress");
  if (activeJobIdx === -1) activeJobIdx = jobs.findIndex((j) => j.status === "queued");
  if (activeJobIdx === -1 && jobs.length > 0) activeJobIdx = jobs.length - 1;

  // Calculate how many job rows we can show
  const maxJobRows = Math.min(jobs.length, Math.max(3, Math.floor((h - 16) / 3)));

  for (let i = 0; i < Math.min(jobs.length, maxJobRows); i++) {
    const job = jobs[i];
    const icon = statusIcon(job.status, job.conclusion, frame);
    const label = statusLabel(job.status, job.conclusion);
    const dur = jobDuration(job);
    const nameStr = truncate(job.name, w - 35);
    const isActive = i === activeJobIdx;
    const highlight = isActive ? ansi.bgHighlight : "";
    const resetHighlight = isActive ? ansi.reset : "";

    const inner = w - 4;
    const left = `${highlight} ${icon} ${nameStr}${resetHighlight}`;
    const right = `${highlight}${ansi.fgCyan}${dur}${ansi.reset}${highlight}  ${statusColor(job.status, job.conclusion)}${label}${ansi.reset}${resetHighlight}`;
    const leftVis = visibleLength(left);
    const rightVis = visibleLength(right);
    const gap = inner - leftVis - rightVis;
    if (gap > 0) {
      lines.push(drawRow(`${left}${" ".repeat(gap)}${right}`, w));
    } else {
      lines.push(drawRow(left, w));
    }
  }

  if (jobs.length > maxJobRows) {
    lines.push(drawRow(`${ansi.dim}  ... ${jobs.length - maxJobRows} more jobs${ansi.reset}`, w));
  }

  // --- Steps section for active job ---
  const activeJob = jobs[activeJobIdx] ?? null;
  if (activeJob && activeJob.steps && activeJob.steps.length > 0) {
    lines.push(drawSectionBorder(`Steps: ${truncate(activeJob.name, w - 20)}`, w));

    // Progress bar for the job
    const completedSteps = activeJob.steps.filter(
      (s) => s.status === "completed"
    ).length;
    const totalSteps = activeJob.steps.length;
    if (activeJob.status === "in_progress") {
      lines.push(drawRow(progressBar(completedSteps, totalSteps, w - 6), w));
    }

    // Calculate available rows for steps
    const usedLines = lines.length;
    const footerLines = 3; // footer section + bottom border + footer row
    const logLines = state.showLogs ? Math.min(8, state.logs.length + 2) : 0;
    const availableForSteps = Math.max(3, h - usedLines - footerLines - logLines - 2);
    const maxStepRows = Math.min(activeJob.steps.length, availableForSteps);

    // Auto-scroll to active step
    let activeStepIdx = activeJob.steps.findIndex((s) => s.status === "in_progress");
    if (activeStepIdx === -1) activeStepIdx = 0;

    let startStep = 0;
    if (activeJob.steps.length > maxStepRows) {
      startStep = Math.max(0, activeStepIdx - Math.floor(maxStepRows / 2));
      startStep = Math.min(startStep, activeJob.steps.length - maxStepRows);
    }

    for (let i = startStep; i < startStep + maxStepRows && i < activeJob.steps.length; i++) {
      const step = activeJob.steps[i];
      const icon = statusIcon(step.status, step.conclusion, frame);
      const dur = stepDuration(step);
      const stepNum = `${step.number}.`.padStart(3);
      const nameStr = truncate(step.name, w - 30);
      const isActive = step.status === "in_progress";
      const highlight = isActive ? ansi.bgHighlight : "";
      const resetHighlight = isActive ? ansi.reset : "";

      const inner = w - 4;
      const left = `${highlight} ${icon} ${ansi.fgGray}${stepNum}${ansi.reset}${highlight} ${nameStr}${resetHighlight}`;
      const right = `${highlight}${ansi.fgCyan}${dur}${ansi.reset}${resetHighlight}`;
      const leftVis = visibleLength(left);
      const rightVis = visibleLength(right);
      const gap = inner - leftVis - rightVis;
      if (gap > 0) {
        lines.push(drawRow(`${left}${" ".repeat(gap)}${right}`, w));
      } else {
        lines.push(drawRow(left, w));
      }
    }

    if (activeJob.steps.length > maxStepRows) {
      const hidden = activeJob.steps.length - maxStepRows;
      lines.push(drawRow(`${ansi.dim}  ... ${hidden} more steps${ansi.reset}`, w));
    }
  }

  // --- Log panel ---
  if (state.showLogs && state.logs.length > 0) {
    const activeStep = activeJob?.steps?.find((s) => s.status === "in_progress");
    const logTitle = activeStep ? `Log: ${truncate(activeStep.name, w - 20)}` : "Logs";
    lines.push(drawSectionBorder(logTitle, w));

    const maxLogLines = Math.min(8, Math.max(3, h - lines.length - 4));
    const logSlice = state.logs.slice(-maxLogLines);
    for (const logLine of logSlice) {
      const trimmed = truncate(logLine, w - 6);
      lines.push(drawRow(`${ansi.dim}${ansi.fgDimWhite}  ${trimmed}${ansi.reset}`, w));
    }
  } else if (state.showLogs) {
    lines.push(drawSectionBorder("Logs", w));
    lines.push(drawRow(`${ansi.dim}No log output available${ansi.reset}`, w));
  }

  // --- Error indicator ---
  if (state.error) {
    lines.push(drawSectionBorder("Error", w));
    lines.push(
      drawRow(`${ansi.fgRed}${truncate(state.error, w - 6)}${ansi.reset}`, w)
    );
  }

  // --- Footer ---
  const refreshInterval = run.status === "queued" ? "10s" : "3s";
  lines.push(drawSectionBorder("", w));
  lines.push(
    drawFooterRow(
      [
        `${ansi.fgCyan}↻ ${refreshInterval}${ansi.reset}`,
        `${ansi.fgDimWhite}q${ansi.reset} quit`,
        `${ansi.fgDimWhite}r${ansi.reset} refresh`,
        `${ansi.fgDimWhite}l${ansi.reset} toggle logs`,
      ],
      w
    )
  );
  lines.push(drawBottomBorder(w));

  // Pad remaining terminal height with empty lines to prevent artifacts
  while (lines.length < h) {
    lines.push("");
  }

  return lines.slice(0, h).join("\n");
}

// ---------------------------------------------------------------------------
// Completion summary
// ---------------------------------------------------------------------------

function renderSummary(state: DashboardState): string {
  const w = state.termWidth;
  const run = state.run!;
  const jobs = state.jobs;
  const lines: string[] = [];

  const isSuccess = run.conclusion === "success";
  const icon = isSuccess ? `${ansi.fgGreen}✓` : `${ansi.bold}${ansi.fgRed}✗`;
  const label = isSuccess ? "Deployment Complete" : "Deployment Failed";
  const totalElapsed = elapsed(run.run_started_at || run.created_at);

  lines.push(drawTopBorder(`${icon} ${label}${ansi.reset}${ansi.fgGray}`, "", w));
  lines.push(drawEmptyRow(w));

  const titleLine = `${run.display_title || run.name} ${isSuccess ? "succeeded" : "failed"} in ${totalElapsed}`;
  lines.push(drawRow(`  ${ansi.bold}${titleLine}${ansi.reset}`, w));
  lines.push(drawEmptyRow(w));

  // Job summary
  const passed = jobs.filter((j) => j.conclusion === "success").length;
  const failed = jobs.filter((j) => j.conclusion === "failure").length;
  const cancelled = jobs.filter((j) => j.conclusion === "cancelled").length;
  const skipped = jobs.filter((j) => j.conclusion === "skipped").length;

  let jobSummary = `  Jobs:  ${ansi.fgGreen}${passed} passed${ansi.reset}`;
  if (failed > 0) jobSummary += `, ${ansi.fgRed}${failed} failed${ansi.reset}`;
  if (cancelled > 0) jobSummary += `, ${ansi.fgYellow}${cancelled} cancelled${ansi.reset}`;
  if (skipped > 0) jobSummary += `, ${ansi.fgGray}${skipped} skipped${ansi.reset}`;
  lines.push(drawRow(jobSummary, w));

  // Individual job results
  for (const job of jobs) {
    const jIcon = statusIcon("completed", job.conclusion, 0);
    const dur = jobDuration(job);
    lines.push(drawRow(`    ${jIcon} ${job.name}  ${ansi.fgCyan}${dur}${ansi.reset}`, w));
  }

  lines.push(drawEmptyRow(w));
  lines.push(drawRow(`  ${ansi.dim}${run.html_url}${ansi.reset}`, w));
  lines.push(drawEmptyRow(w));
  lines.push(drawBottomBorder(w));

  return lines.join("\n");
}

// ---------------------------------------------------------------------------
// Non-TTY fallback
// ---------------------------------------------------------------------------

function renderPlain(state: DashboardState): string {
  if (!state.run) {
    return state.error ?? "Loading...";
  }
  const run = state.run;
  const jobs = state.jobs;
  const lines: string[] = [];

  lines.push(`[Deploy Watch] ${run.display_title || run.name} (run #${run.run_number})`);
  lines.push(`Status: ${run.status}${run.conclusion ? ` (${run.conclusion})` : ""}`);
  lines.push(`Elapsed: ${elapsed(run.run_started_at || run.created_at)}`);
  lines.push("");

  for (const job of jobs) {
    const icon = job.conclusion === "success" ? "PASS" : job.conclusion === "failure" ? "FAIL" : job.status.toUpperCase();
    lines.push(`  [${icon}] ${job.name} (${jobDuration(job)})`);
  }

  return lines.join("\n");
}

// ---------------------------------------------------------------------------
// Main application
// ---------------------------------------------------------------------------

async function main() {
  // Parse arguments
  const args = process.argv.slice(2);
  let repo = process.env.DEPLOY_WATCH_REPO ?? "MadAppGang/magmux";
  let runId: number | undefined;

  for (const arg of args) {
    if (arg === "--help" || arg === "-h") {
      console.log(`Usage: deploy-watch.ts [options] [run-id]

Options:
  --repo <owner/repo>   GitHub repository (default: MadAppGang/magmux)
  --help, -h            Show this help

Environment:
  DEPLOY_WATCH_REPO     Default repository

Controls:
  q, Ctrl-C   Quit
  r            Force refresh
  l            Toggle log panel
  j/k, ↑/↓    Scroll jobs

If no run ID is given, auto-detects the latest in-progress run.`);
      process.exit(0);
    }
    if (arg === "--repo") continue;
    if (args[args.indexOf(arg) - 1] === "--repo") {
      repo = arg;
      continue;
    }
    // Treat as run ID
    const parsed = parseInt(arg, 10);
    if (!isNaN(parsed)) {
      runId = parsed;
    }
  }

  // Check if TTY
  const isTTY = process.stdout.isTTY ?? false;

  // Check gh is available
  try {
    const ghCheck = Bun.spawn(["gh", "auth", "status"], {
      stdout: "pipe",
      stderr: "pipe",
    });
    const exitCode = await ghCheck.exited;
    if (exitCode !== 0) {
      const stderr = await new Response(ghCheck.stderr).text();
      console.error("Error: gh CLI is not authenticated.");
      console.error("Run: gh auth login");
      if (stderr) console.error(stderr);
      process.exit(1);
    }
  } catch {
    console.error("Error: gh CLI is not installed.");
    console.error("Install: https://cli.github.com/");
    process.exit(1);
  }

  // Initialize state
  const state: DashboardState = {
    run: null,
    jobs: [],
    logs: [],
    error: null,
    lastFetch: 0,
    showLogs: true,
    selectedJobIdx: 0,
    scrollOffset: 0,
    spinnerFrame: 0,
    termWidth: process.stdout.columns ?? 80,
    termHeight: process.stdout.rows ?? 24,
  };

  // --- Non-TTY mode: single fetch, print, exit ---
  if (!isTTY) {
    const result = runId ? await fetchRun(repo, runId) : await fetchLatestRun(repo);
    if (!result.ok || !result.run) {
      console.error(result.error ?? "No runs found");
      process.exit(1);
    }
    state.run = result.run;
    const jobsResult = await fetchJobs(repo, result.run.id);
    state.jobs = jobsResult.jobs ?? [];
    console.log(renderPlain(state));
    process.exit(0);
  }

  // --- TTY mode: interactive dashboard ---

  // Enter alternate screen, hide cursor
  process.stdout.write(ansi.altScreenOn + ansi.cursorHide);

  let running = true;
  let forceRefresh = false;

  // Clean exit handler
  function cleanup() {
    running = false;
    process.stdout.write(ansi.altScreenOff + ansi.cursorShow);
    // Restore stdin if raw
    if (process.stdin.isTTY && typeof process.stdin.setRawMode === "function") {
      try {
        process.stdin.setRawMode(false);
      } catch {
        // ignore
      }
    }
  }

  process.on("SIGINT", () => {
    cleanup();
    process.exit(0);
  });
  process.on("SIGTERM", () => {
    cleanup();
    process.exit(0);
  });

  // Handle terminal resize
  process.on("SIGWINCH", () => {
    state.termWidth = process.stdout.columns ?? 80;
    state.termHeight = process.stdout.rows ?? 24;
  });

  // Enable raw mode for keypress handling
  if (process.stdin.isTTY && typeof process.stdin.setRawMode === "function") {
    process.stdin.setRawMode(true);
  }

  // Read keypresses as a stream
  const stdinReader = (async () => {
    const decoder = new TextDecoder();
    for await (const chunk of process.stdin as AsyncIterable<Uint8Array>) {
      if (!running) break;
      const key = decoder.decode(chunk);

      // Ctrl-C
      if (key === "\x03") {
        cleanup();
        process.exit(0);
      }
      // q
      if (key === "q" || key === "Q") {
        cleanup();
        process.exit(0);
      }
      // r - force refresh
      if (key === "r" || key === "R") {
        forceRefresh = true;
      }
      // l - toggle logs
      if (key === "l" || key === "L") {
        state.showLogs = !state.showLogs;
      }
      // j / down arrow - scroll down
      if (key === "j" || key === "\x1b[B") {
        state.selectedJobIdx = Math.min(state.selectedJobIdx + 1, state.jobs.length - 1);
      }
      // k / up arrow - scroll up
      if (key === "k" || key === "\x1b[A") {
        state.selectedJobIdx = Math.max(state.selectedJobIdx - 1, 0);
      }
    }
  })();

  // Fetch data function
  async function fetchData() {
    try {
      // Fetch run
      const runResult = runId
        ? await fetchRun(repo, runId)
        : state.run
          ? await fetchRun(repo, state.run.id)
          : await fetchLatestRun(repo);

      if (runResult.ok && runResult.run) {
        state.run = runResult.run;
        if (!runId) runId = runResult.run.id; // Lock to this run once found
        state.error = null;

        // Fetch jobs
        const jobsResult = await fetchJobs(repo, runResult.run.id);
        if (jobsResult.ok && jobsResult.jobs) {
          state.jobs = jobsResult.jobs;
        }

        // Fetch logs for active job
        if (state.showLogs) {
          const activeJob = state.jobs.find((j) => j.status === "in_progress") ?? state.jobs[state.jobs.length - 1];
          if (activeJob) {
            const logs = await fetchJobLogs(repo, activeJob.id);
            if (logs.length > 0) state.logs = logs;
          }
        }
      } else {
        state.error = runResult.error ?? "Failed to fetch run";
      }
    } catch (e: any) {
      state.error = `Fetch error: ${e.message ?? String(e)}`;
    }
    state.lastFetch = Date.now();
  }

  // Initial fetch
  await fetchData();

  // Main loop
  const spinnerInterval = 150; // ms per spinner frame
  let lastSpinnerTick = Date.now();

  while (running) {
    // Update terminal size
    state.termWidth = Math.max(40, process.stdout.columns ?? 80);
    state.termHeight = Math.max(10, process.stdout.rows ?? 24);

    // Update spinner
    const now = Date.now();
    if (now - lastSpinnerTick >= spinnerInterval) {
      state.spinnerFrame = (state.spinnerFrame + 1) % SPINNER_FRAMES.length;
      lastSpinnerTick = now;
    }

    // Determine refresh interval
    const refreshMs =
      state.run?.status === "queued" ? 10_000 : 3_000;

    // Check if we need to fetch
    if (forceRefresh || now - state.lastFetch >= refreshMs) {
      forceRefresh = false;
      await fetchData();
    }

    // Check if workflow completed
    if (state.run?.status === "completed") {
      // One final fetch to get complete data
      if (now - state.lastFetch > 1000) {
        await fetchData();
      }
      // Show summary
      process.stdout.write(ansi.clear + renderSummary(state));
      // Wait for keypress to exit
      await Bun.sleep(100);
      // Give the user a moment to read, then auto-exit after 30s
      const summaryStart = Date.now();
      while (running && Date.now() - summaryStart < 30_000) {
        await Bun.sleep(100);
      }
      break;
    }

    // Render dashboard
    const output = render(state);
    process.stdout.write(ansi.moveTo(1, 1) + output);

    // Small sleep to avoid busy loop
    await Bun.sleep(50);
  }

  cleanup();
}

// ---------------------------------------------------------------------------
// Entry point
// ---------------------------------------------------------------------------

main().catch((e) => {
  // Make sure we restore terminal on crash
  if (process.stdout.isTTY) {
    process.stdout.write(ansi.altScreenOff + ansi.cursorShow);
    if (process.stdin.isTTY && typeof process.stdin.setRawMode === "function") {
      try {
        process.stdin.setRawMode(false);
      } catch {
        // ignore
      }
    }
  }
  console.error("Fatal error:", e);
  process.exit(1);
});
