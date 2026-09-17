#!/usr/bin/env bun
/**
 * The terminal half of the remote-control demo: an ANSI `Painter` on
 * `client.ts`, and the entry point that wires one to the other.
 *
 *   bun demo/rc/cli.ts --url http://127.0.0.1:PORT --token-file /path/view.token
 *
 * Flags
 *   --url URL           magmux's --listen address (required)
 *   --token-file PATH   the token to present; read off disk, the operator path
 *   --pane N            which pane to watch (default: the first session pane)
 *   --fps N             frames per second to ask for, 1..30 (default 15)
 *   --no-keys           do not read stdin at all (no pane switching)
 *   --verbose           log the message sequence to stderr as it happens
 *
 * Keys, when stdin is a terminal: `]` and `[` move to the next and previous
 * pane, `0`..`9` jump to that pane id, `q` quits. They are the CLIENT's own
 * act — `unwatch` then `watch` on its own connection — because a watch is
 * per-subscriber state and there is deliberately no op that retargets somebody
 * else's stream. See `RemoteClient.retarget`.
 *
 * The token is read from a FILE and never taken on the command line: a value on
 * a command line is in every `ps` listing on the machine, which is the same
 * reason magmux itself has `--token-file` and no `--token`.
 *
 * Everything below the argument parsing is painting. The protocol lives in
 * `client.ts` and the decode in `frame.ts`; this file knows about SGR, the
 * alternate screen and the size of a terminal, and about nothing else.
 */
import fs from "node:fs";
import process from "node:process";

import { RemoteClient, type Painter, type Status } from "./client.ts";
import {
  ATTR_BLINK,
  ATTR_BOLD,
  ATTR_DIM,
  ATTR_INVIS,
  ATTR_ITALIC,
  ATTR_OVERLINE,
  ATTR_REVERSE,
  ATTR_STRIKE,
  ATTR_UNDERLINE,
  classifyColour,
  rowSpans,
  type CellCols,
  type Cursor,
  type Line,
  type Span,
} from "./frame.ts";

// ── ANSI, which is this file's whole subject ────────────────────────────────

const ESC = "\x1b[";
const ALT_ON = `${ESC}?1049h`;
const ALT_OFF = `${ESC}?1049l`;
const CURSOR_HIDE = `${ESC}?25l`;
const CURSOR_SHOW = `${ESC}?25h`;
const SGR_RESET = `${ESC}0m`;

/** Move to a 0-based row and column. */
function at(y: number, x: number): string {
  return `${ESC}${y + 1};${x + 1}H`;
}

/**
 * One span's style as an SGR sequence.
 *
 * Every span is emitted as a full reset plus its own attributes rather than as
 * a diff against the span before it. A diff would be fewer bytes and one more
 * piece of state to get wrong, and the frames are already coalesced by magmux —
 * the bytes were never the bottleneck.
 *
 * The colour cases come from `frame.ts`'s CLASSIFICATION, which is the shared
 * part; turning `{kind:"indexed"}` into `38;5;N` is this surface's own business
 * and the web painter turns the same value into a CSS colour.
 */
function sgr(span: Span): string {
  const parts: number[] = [0];
  const a = span.attr;
  if (a & ATTR_BOLD) parts.push(1);
  if (a & ATTR_DIM) parts.push(2);
  if (a & ATTR_ITALIC) parts.push(3);
  if (a & ATTR_UNDERLINE) parts.push(4);
  if (a & ATTR_BLINK) parts.push(5);
  if (a & ATTR_REVERSE) parts.push(7);
  if (a & ATTR_INVIS) parts.push(8);
  if (a & ATTR_STRIKE) parts.push(9);
  if (a & ATTR_OVERLINE) parts.push(53);

  const fg = classifyColour(span.fg);
  if (fg.kind === "indexed") {
    if (fg.index < 8) parts.push(30 + fg.index);
    else if (fg.index < 16) parts.push(90 + (fg.index - 8));
    else parts.push(38, 5, fg.index);
  } else if (fg.kind === "rgb") {
    parts.push(38, 2, fg.r, fg.g, fg.b);
  }

  const bg = classifyColour(span.bg);
  if (bg.kind === "indexed") {
    if (bg.index < 8) parts.push(40 + bg.index);
    else if (bg.index < 16) parts.push(100 + (bg.index - 8));
    else parts.push(48, 5, bg.index);
  } else if (bg.kind === "rgb") {
    parts.push(48, 2, bg.r, bg.g, bg.b);
  }
  return `${ESC}${parts.join(";")}m`;
}

/**
 * Take at most `avail` CELL columns of a span, never a code point more.
 *
 * Clipping in cells rather than in characters is the whole of R-2: a wide glyph
 * cut in half at the right edge either wraps — shifting every row below it, so
 * the picture looks corrupt rather than clipped — or is drawn in one column and
 * pushes the rest of the row left. The widths come from the line's own `wd`,
 * through `cells`, and never from a width table of this client's own.
 */
function clipSpan(span: Span, avail: number, cells: CellCols): string {
  if (avail <= 0) return "";
  if (span.width <= avail) return span.text;
  let out = "";
  let used = 0;
  let cp = span.col < cells.cpAt.length ? cells.cpAt[span.col] : -1;
  for (const ch of span.text) {
    const wide = cp >= 0 && cp < cells.cps.length && cells.colOf[cp + 1] - cells.colOf[cp] === 2;
    const w = wide ? 2 : 1;
    if (used + w > avail) break;
    out += ch;
    used += w;
    if (cp >= 0 && ++cp >= cells.cps.length) cp = -1;
  }
  return out;
}

// ── the painter ─────────────────────────────────────────────────────────────

const STATUS_ROWS = 1;

class ANSIPainter implements Painter {
  private out: string[] = [];
  private flushing = false;
  private altOn = false;
  private rows = 0;
  private cols = 0;
  private lastCursor: Cursor = { y: 0, x: 0, vis: false };
  private lastStatus: Status | null = null;
  private clipNoticeSaid = false;
  private notice = "";
  private noticeUntil = 0;
  /** Whether the key hints are worth a place in the status bar at all. */
  keysOn = false;

  /**
   * Say something in the status bar for a few seconds.
   *
   * A key that does nothing is indistinguishable from a broken one, so a
   * refused pane switch has to be visible — and the status bar is the only
   * place this client owns. It expires, because a stale notice sitting under a
   * live picture is its own small lie.
   */
  say(msg: string) {
    this.notice = msg;
    this.noticeUntil = Date.now() + 4000;
    if (this.lastStatus) this.status(this.lastStatus);
  }

  constructor(private readonly write: (s: string) => void = (s) => process.stdout.write(s)) {}

  /** Terminal geometry, re-read every frame: a pane can be resized under us. */
  private term() {
    return {
      cols: process.stdout.columns ?? 80,
      rows: process.stdout.rows ?? 24,
    };
  }

  private push(s: string) {
    this.out.push(s);
    if (this.flushing) return;
    this.flushing = true;
    // One write per frame. Every row of a frame is painted synchronously, so a
    // microtask flush coalesces them; a write per row would tear visibly on a
    // full repaint of a 30-row pane.
    queueMicrotask(() => {
      this.flushing = false;
      const s = this.out.join("");
      this.out.length = 0;
      if (s) this.write(s);
    });
  }

  /**
   * The size guard.
   *
   * The pane's geometry is magmux's, not ours, and a terminal smaller than the
   * pane has only two honest options: clip, or wrap. Wrapping shifts every row
   * below the overflow and reads as corruption rather than as a small window,
   * so this client CLIPS — and says so once, in one line naming both sizes, so
   * the viewer is never left wondering whether the missing columns are a bug in
   * the mirror.
   */
  size(rows: number, cols: number) {
    this.rows = rows;
    this.cols = cols;
    const t = this.term();
    if (!this.clipNoticeSaid && (t.cols < cols || t.rows < rows + STATUS_ROWS)) {
      this.clipNoticeSaid = true;
      // Said BEFORE the alternate screen is entered, so it survives on the
      // normal screen and is still there after this client exits. The status
      // bar repeats it for the duration of the run.
      process.stderr.write(
        `\x1b[33mmagmux-rc: this terminal is ${t.cols}x${t.rows} and the pane is ` +
          `${cols}x${rows} (+${STATUS_ROWS} status row) — clipping, not wrapping\x1b[0m\n`,
      );
    }
    if (!this.altOn) {
      this.altOn = true;
      this.push(ALT_ON + CURSOR_HIDE + `${ESC}2J`);
    }
  }

  clear() {
    this.push(`${ESC}2J` + at(0, 0));
  }

  row(y: number, line: Line, cells: CellCols) {
    const t = this.term();
    const viewRows = Math.min(this.rows, Math.max(0, t.rows - STATUS_ROWS));
    if (y >= viewRows) return; // clipped away entirely
    const viewCols = Math.min(this.cols, t.cols);

    let buf = at(y, 0);
    let col = 0;
    for (const span of rowSpans(line, cells, this.cols)) {
      if (col >= viewCols) break;
      const text = clipSpan(span, viewCols - col, cells);
      if (!text) break;
      buf += sgr(span) + text;
      col += span.width;
    }
    // Reset FIRST, then erase: an EL with a background colour still set would
    // paint the rest of the line in that colour.
    buf += SGR_RESET + `${ESC}K`;
    this.push(buf);
  }

  cursor(c: Cursor) {
    this.lastCursor = c;
    this.paintCursor();
  }

  private paintCursor() {
    const t = this.term();
    const c = this.lastCursor;
    const viewRows = Math.min(this.rows, Math.max(0, t.rows - STATUS_ROWS));
    if (!c.vis || c.y >= viewRows || c.x >= Math.min(this.cols, t.cols)) {
      this.push(CURSOR_HIDE);
      return;
    }
    this.push(at(c.y, c.x) + CURSOR_SHOW);
  }

  status(s: Status) {
    this.lastStatus = s;
    if (!this.altOn) return;
    const t = this.term();
    const colour = s.state === "live" ? "\x1b[42;30m" : s.state === "stale" ? "\x1b[43;30m" : "\x1b[41;97m";
    const notice = Date.now() < this.noticeUntil ? this.notice : "";
    const text = statusText(s, this.rows, this.cols, t, this.keysOn, notice);
    this.push(
      at(t.rows - 1, 0) + SGR_RESET + colour + text.slice(0, t.cols).padEnd(t.cols) + SGR_RESET,
    );
    this.paintCursor();
  }

  end(reason: string, ok: boolean) {
    // Leave the alternate screen and give the cursor back BEFORE anything is
    // printed: a message written inside the alternate screen disappears with
    // it, which is the same as not printing it.
    const tail = this.altOn ? SGR_RESET + CURSOR_SHOW + ALT_OFF : SGR_RESET + CURSOR_SHOW;
    this.altOn = false;
    this.out.push(tail);
    const s = this.out.join("");
    this.out.length = 0;
    if (s) this.write(s);
    const st = this.lastStatus;
    const seen = st ? ` after ${st.frames} frames (seq ${st.seq})` : "";
    if (ok) {
      process.stderr.write(`\x1b[32mmagmux-rc: ${reason}${seen}\x1b[0m\n`);
    } else {
      process.stderr.write(`\x1b[31mmagmux-rc: ${reason}${seen}\x1b[0m\n`);
    }
  }
}

function statusText(
  s: Status,
  rows: number,
  cols: number,
  t: { rows: number; cols: number },
  keysOn: boolean,
  notice: string,
): string {
  const bits: string[] = [];
  const dot = s.state === "live" ? "●" : s.state === "stale" ? "◐" : "✗";
  bits.push(`${dot} ${s.state}`);
  bits.push(`pane ${s.pane}`);
  // EARLY, because the bar is truncated to the terminal's width and a hint
  // nobody can see is not a hint. It advertises itself only when it can do
  // something: one pane and no keyboard would make `[ ]` an instruction to
  // press a key that is refused.
  const watchable = s.panes.filter((p) => p.state !== "panel");
  if (keysOn && watchable.length > 1) {
    bits.push(`[ ] ${watchable.map((p) => (p.pane === s.pane ? `<${p.pane}>` : `${p.pane}`)).join(" ")}`);
  }
  bits.push(`${cols}x${rows}`);
  if (t.cols < cols || t.rows < rows + STATUS_ROWS) bits.push(`CLIPPED to ${t.cols}x${Math.max(0, t.rows - STATUS_ROWS)}`);
  bits.push(`seq ${s.seq}`);
  bits.push(`${s.frames} frames`);
  if (s.fps) bits.push(`${s.fps} fps`);
  bits.push(s.ageMs < 0 ? "no frame yet" : `last frame ${(s.ageMs / 1000).toFixed(1)}s ago`);
  if (s.alt) bits.push("alt");
  if (s.scrolled) bits.push("local viewer scrolled back");
  if (notice) bits.push(notice);
  if (s.reason) bits.push(s.reason);
  return " " + bits.join(" · ");
}

// ── entry point ─────────────────────────────────────────────────────────────

interface Args {
  url: string;
  tokenFile: string;
  pane: number | null;
  fps: number;
  verbose: boolean;
  keys: boolean;
}

function parseArgs(argv: string[]): Args {
  const a: Args = { url: "", tokenFile: "", pane: null, fps: 15, verbose: false, keys: true };
  for (let i = 0; i < argv.length; i++) {
    switch (argv[i]) {
      case "--url":
        a.url = argv[++i] ?? "";
        break;
      case "--token-file":
        a.tokenFile = argv[++i] ?? "";
        break;
      case "--pane":
        a.pane = Number(argv[++i]);
        break;
      case "--fps":
        a.fps = Number(argv[++i]);
        break;
      case "--no-keys":
        a.keys = false;
        break;
      case "--verbose":
      case "-v":
        a.verbose = true;
        break;
      case "--help":
      case "-h":
        usage();
        process.exit(0);
      default:
        die(`unknown argument ${argv[i]}`);
    }
  }
  if (!a.url) die("--url is required (magmux's --listen address)");
  if (!a.tokenFile) die("--token-file is required");
  if (a.pane !== null && !Number.isInteger(a.pane)) die("--pane must be an integer");
  if (!Number.isFinite(a.fps) || a.fps < 1 || a.fps > 30) die("--fps must be 1..30");
  return a;
}

function usage() {
  process.stdout.write(
    [
      "magmux remote-control demo — terminal client",
      "",
      "  bun demo/rc/cli.ts --url http://127.0.0.1:PORT --token-file PATH [options]",
      "",
      "  --url URL          magmux's --listen address",
      "  --token-file PATH  file holding the token to present (never on argv)",
      "  --pane N           pane to watch (default: the first session pane)",
      "  --fps N            frames per second to ask for, 1..30 (default 15)",
      "  --no-keys          do not read stdin; no pane switching",
      "  --verbose, -v      log the message sequence to stderr",
      "",
      "  Keys (when stdin is a terminal): ] next pane, [ previous, 0-9 by id, q quit.",
      "",
    ].join("\n"),
  );
}

function die(msg: string): never {
  process.stderr.write(`\x1b[31mmagmux-rc: ${msg}\x1b[0m\n`);
  process.exit(1);
}

if (import.meta.main) {
  const args = parseArgs(process.argv.slice(2));

  let token = "";
  try {
    token = fs.readFileSync(args.tokenFile, "utf8").trim();
  } catch (err: any) {
    die(`cannot read ${args.tokenFile}: ${err?.message ?? err}`);
  }
  if (!token) die(`${args.tokenFile} is empty`);

  const painter = new ANSIPainter();
  const client = new RemoteClient(
    {
      url: args.url,
      token,
      pane: args.pane,
      fps: args.fps,
      client: "magmux-rc-demo/cli",
      verbose: args.verbose,
      log: (m) => process.stderr.write(`\x1b[90m${m}\x1b[0m\n`),
    },
    painter,
  );

  // The only two ways out other than the stream ending. Both go through
  // `quit`, so the pane is unwatched before the socket closes — magmux keeps
  // no framer for a pane nobody watches, and leaving one running for a client
  // that has gone is exactly the cost the watcher count exists to avoid.
  for (const sig of ["SIGINT", "SIGTERM"] as const) {
    process.on(sig, () => client.quit(`ended by ${sig}`));
  }

  if (args.keys) wireKeys(client, painter);

  const outcome = await client.run();
  process.exit(outcome.ok ? 0 : 1);
}

/**
 * The pane switcher: this client's own keyboard, driving its own connection.
 *
 * Raw mode is what makes a single keypress an action rather than a line the
 * viewer has to submit, and it is entered ONLY when stdin is a terminal — the
 * selftest and anything piping into this client are not, and a `setRawMode` on
 * a pipe throws. It is restored on `exit` rather than in the painter, because
 * the painter does not own stdin and a terminal left in raw mode outlives the
 * process that did it.
 *
 * Ctrl-C arrives here as the BYTE 0x03, not as SIGINT: raw mode turns off the
 * terminal's signal generation, so the handler registered above would never
 * fire and the viewer would be stuck in a client that could not be interrupted.
 */
function wireKeys(client: RemoteClient, painter: ANSIPainter) {
  const stdin = process.stdin as NodeJS.ReadStream;
  if (!stdin.isTTY) return;
  try {
    stdin.setRawMode(true);
  } catch {
    return; // not something this terminal allows; the mirror still works
  }
  painter.keysOn = true;
  stdin.resume();
  stdin.setEncoding("utf8");
  process.on("exit", () => {
    try {
      stdin.setRawMode(false);
    } catch {}
  });

  const refused = (why: string | null, did: string) => painter.say(why ? `refused: ${why}` : did);

  stdin.on("data", (chunk: string) => {
    for (const ch of chunk) {
      if (ch === "\x03" || ch === "q") {
        client.quit(ch === "\x03" ? "ended by Ctrl-C in the mirror" : "ended by q in the mirror");
        return;
      }
      if (ch === "]") refused(client.cycle(1), `watching pane ${client.watchingPane()}`);
      else if (ch === "[") refused(client.cycle(-1), `watching pane ${client.watchingPane()}`);
      else if (ch >= "0" && ch <= "9") refused(client.retarget(Number(ch)), `watching pane ${ch}`);
    }
  });
}

export { ANSIPainter, clipSpan, sgr };
