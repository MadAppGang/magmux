/**
 * Socket bridge to a running magmux.
 *
 * magmux exports MAGMUX_SOCK to every pane it spawns, so a pilot running as a
 * pane finds its multiplexer with no configuration. The socket speaks
 * line-delimited JSON in both directions: magmux pushes `snapshot` / `exit` /
 * `control` / `results` events out, and accepts `send` / `pilot` messages in.
 *
 * The one piece of real logic here is `runInstruction`. Everything else is
 * plumbing.
 */
import net from "node:net";
import { EventEmitter } from "node:events";

export interface MagmuxEvent {
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
  exitCode?: number;
  [k: string]: unknown;
}

/** What the controlled session did in response to one instruction. */
export interface TurnResult {
  /** Terminal state the pane settled in. */
  state: "awaiting_input" | "awaiting_permission" | "error" | "gone" | "stalled";
  /** The session's last response text, as magmux observed it. */
  response: string;
  /** Last tool the session used during the turn, if any. */
  tool: string;
  durationMs: number;
  /** True if the turn never visibly started, or never finished in time. */
  stalled: boolean;
}

/** States in which the session is finished with a turn and can take another. */
const SETTLED = new Set(["awaiting_input", "awaiting_permission", "error", "gone"]);

/**
 * The aggregate snapshot uses magmux's pane-level vocabulary (built from
 * dead/exitCode/inputReady) rather than the controller lifecycle names the
 * live snapshots carry. Translate so the pilot only reasons about one set.
 */
function aggregateState(s: string): string {
  switch (s) {
    case "awaiting_input":
      return "awaiting_input";
    case "completed":
    case "failed":
      return "gone";
    case "running":
      return "working";
    default:
      return s;
  }
}

export interface BridgeOptions {
  /** Pane index of the session being driven. */
  pane: number;
  /** How long to wait for a turn to visibly begin after an instruction. */
  startTimeoutMs?: number;
  /** How long to wait for a started turn to finish. */
  turnTimeoutMs?: number;
}

export class MagmuxBridge extends EventEmitter {
  private conn: net.Socket | null = null;
  private buf = "";
  private closed = false;

  /** Last state magmux reported for the controlled pane. */
  state = "unknown";
  response = "";
  tool = "";
  model = "";

  readonly pane: number;
  private readonly startTimeoutMs: number;
  private readonly turnTimeoutMs: number;

  constructor(readonly sockPath: string, opts: BridgeOptions) {
    super();
    this.pane = opts.pane;
    this.startTimeoutMs = opts.startTimeoutMs ?? 45_000;
    this.turnTimeoutMs = opts.turnTimeoutMs ?? 15 * 60_000;
  }

  connect(): Promise<void> {
    return new Promise((resolve, reject) => {
      const conn = net.createConnection(this.sockPath);
      this.conn = conn;
      conn.on("connect", () => resolve());
      conn.on("error", (e) => {
        if (this.closed) return;
        reject(e);
        this.emit("closed");
      });
      conn.on("end", () => {
        this.closed = true;
        this.emit("closed");
      });
      conn.on("data", (chunk) => this.ingest(chunk.toString()));
    });
  }

  private ingest(chunk: string) {
    this.buf += chunk;
    let nl: number;
    while ((nl = this.buf.indexOf("\n")) >= 0) {
      const line = this.buf.slice(0, nl).trim();
      this.buf = this.buf.slice(nl + 1);
      if (!line) continue;
      let ev: MagmuxEvent;
      try {
        ev = JSON.parse(line);
      } catch {
        continue;
      }
      // The connect-time aggregate (`panes`) is the only event a pilot that
      // connects late will ever see for an already-idle session: magmux only
      // pushes per-pane snapshots on *change*, so a session sitting at
      // awaiting_input emits nothing further. Seeding from the aggregate is
      // what stops the pilot waiting forever for a state it already missed.
      if (ev.type === "snapshot" && Array.isArray(ev.panes)) {
        for (const entry of ev.panes as Array<Record<string, unknown>>) {
          if (entry.pane !== this.pane) continue;
          this.state = aggregateState(String(entry.state ?? ""));
          if (typeof entry.response === "string") this.response = entry.response;
          if (typeof entry.model === "string") this.model = entry.model;
        }
      }

      // Per-pane live snapshots carry `pane`; the aggregate carries `panes`
      // instead. Only the former tracks a turn.
      if (ev.type === "snapshot" && ev.pane === this.pane && ev.panes === undefined) {
        this.state = ev.state ?? this.state;
        if (ev.response) this.response = ev.response;
        this.tool = ev.tool ?? "";
        if (ev.model) this.model = ev.model;
      }
      if (ev.type === "exit" && ev.pane === this.pane) {
        this.state = "gone";
      }
      this.emit("event", ev);
    }
  }

  send(msg: Record<string, unknown>) {
    if (!this.conn || this.closed) return;
    this.conn.write(JSON.stringify(msg) + "\n");
  }

  /** Tell magmux which pane this pilot drives and what it is trying to do. */
  announce(goal: string, steps: number, model: string) {
    this.send({ type: "pilot", event: "start", pane: this.pane, goal, steps, model });
  }

  finish(summary: string, failed = false) {
    this.send({ type: "pilot", event: failed ? "fail" : "finish", summary });
  }

  note(label: string, text: string) {
    this.send({ type: "pilot", event: "note", label, text });
  }

  /**
   * Push an instruction into the session and wait for the resulting turn.
   *
   * The wait is two-phase on purpose. The session is already sitting in
   * `awaiting_input` when we send — that is why we are sending — so simply
   * waiting for `awaiting_input` would return instantly with the *previous*
   * turn's response and the pilot would happily plan its next step against a
   * stale answer. So we first wait for the pane to visibly leave the settled
   * state (the turn started), and only then wait for it to settle again.
   *
   * A turn that never starts is reported as stalled rather than silently
   * treated as an empty success, because "the instruction was dropped" and
   * "the session had nothing to do" need different responses from the pilot.
   */
  async runInstruction(text: string, label?: string): Promise<TurnResult> {
    const startedAt = Date.now();
    const before = this.response;

    this.send({ type: "send", pane: this.pane, text, label, enter: true });

    const started = await this.waitFor(
      () => !SETTLED.has(this.state),
      this.startTimeoutMs,
    );
    if (!started) {
      // One honest escape hatch: if the response text changed while we were
      // waiting, the turn did run and we simply never sampled a non-settled
      // state (a turn shorter than the controller's 250ms poll).
      if (this.response !== before) {
        return {
          state: this.state as TurnResult["state"],
          response: this.response,
          tool: this.tool,
          durationMs: Date.now() - startedAt,
          stalled: false,
        };
      }
      return {
        state: "stalled",
        response: "",
        tool: "",
        durationMs: Date.now() - startedAt,
        stalled: true,
      };
    }

    const settled = await this.waitFor(() => SETTLED.has(this.state), this.turnTimeoutMs);
    return {
      state: settled ? (this.state as TurnResult["state"]) : "stalled",
      response: this.response,
      tool: this.tool,
      durationMs: Date.now() - startedAt,
      stalled: !settled,
    };
  }

  /** Wait until `pred` holds, re-checking on every magmux event. */
  private waitFor(pred: () => boolean, timeoutMs: number): Promise<boolean> {
    if (pred()) return Promise.resolve(true);
    return new Promise((resolve) => {
      const done = (ok: boolean) => {
        clearTimeout(timer);
        this.off("event", onEvent);
        this.off("closed", onClosed);
        resolve(ok);
      };
      const onEvent = () => {
        if (pred()) done(true);
      };
      const onClosed = () => done(pred());
      const timer = setTimeout(() => done(false), timeoutMs);
      this.on("event", onEvent);
      this.on("closed", onClosed);
    });
  }

  /** Wait for the session to be ready for its first instruction. */
  async waitUntilReady(timeoutMs = 120_000): Promise<boolean> {
    return this.waitFor(() => this.state === "awaiting_input", timeoutMs);
  }

  close() {
    this.closed = true;
    try {
      this.conn?.destroy();
    } catch {}
  }
}
