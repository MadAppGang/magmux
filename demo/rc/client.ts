/**
 * The ONE connection core for both demo clients.
 *
 * It owns everything between the token and a decoded row: authentication, the
 * WebSocket, the message sequence, the liveness probe, and the end of the run.
 * It paints nothing. Two `Painter` implementations sit on it — `cli.ts` writes
 * ANSI, `web/app.ts` writes DOM — and neither contains a line of protocol.
 *
 * THE RULE: `Painter.row()` takes a DECODED row and its cell columns, never an
 * ANSI string. The moment it takes a string the web painter has to become a
 * terminal emulator, and the whole reason these two clients share a decoder is
 * gone.
 *
 * It uses ONLY globals that bun and the browser both have — `WebSocket`,
 * `fetch`, `setInterval`, `AbortController`, `JSON`. **A `node:` import in this
 * file is a design break, not a detail**: `serve.ts` bundles this exact source
 * for the browser, so anything platform-specific here fails at Bun.build time
 * at best and at run time in a browser at worst.
 */
import {
  applyFrame,
  cellCols,
  newScreen,
  type CellCols,
  type Cursor,
  type FrameMessage,
  type Line,
  type Screen,
} from "./frame.ts";

// ── the port ────────────────────────────────────────────────────────────────

export type State = "connecting" | "live" | "stale" | "dead" | "ended";

/**
 * One pane as this connection knows it: from the connect-time aggregate, then
 * kept in step by `pane_opened` and `pane_closed`.
 *
 * It is deliberately three fields and not the aggregate's whole pane object. A
 * picker needs an id, something to call it, and enough to grey out the control
 * panel; anything more is the aggregate's business and a client that mirrored
 * it would be maintaining a second copy of magmux's state model.
 */
export interface PaneInfo {
  pane: number;
  label: string;
  state: string;
}

export interface Status {
  state: State;
  /** Why, when the state is not `live`. Shown verbatim; never invented. */
  reason: string;
  pane: number;
  seq: number;
  frames: number;
  /** The rate magmux settled on after clamping, from the watch reply. */
  fps: number;
  /** Milliseconds since the last frame, or -1 before the first one. */
  ageMs: number;
  rows: number;
  cols: number;
  /** The local human has scrolled this pane back; the stream is still live. */
  scrolled: boolean;
  alt: boolean;
  /** Every pane this connection knows about, for a picker. Never empty once connected. */
  panes: PaneInfo[];
}

export interface Painter {
  /** From the watch reply, before any frame. A client sizes itself HERE. */
  size(rows: number, cols: number): void;
  /** A keyframe arrived: forget everything before repainting. */
  clear(): void;
  row(y: number, line: Line, cells: CellCols): void;
  cursor(c: Cursor): void;
  status(s: Status): void;
  /** The run is over. `ok` false means it failed and the exit code is 1. */
  end(reason: string, ok: boolean): void;
}

export interface ClientOptions {
  /** `http://127.0.0.1:PORT` — magmux's `--listen` address. */
  url: string;
  token: string;
  /** Which pane to watch. null takes the first non-panel pane in the aggregate. */
  pane?: number | null;
  fps?: number;
  /** What the control panel calls this client. */
  client?: string;
  verbose?: boolean;
  log?: (msg: string) => void;
}

export interface Outcome {
  ok: boolean;
  reason: string;
}

// ── tuning, stated once ─────────────────────────────────────────────────────

/**
 * The liveness probe.
 *
 * SILENCE IS NEVER THE SIGNAL. An idle pane legitimately produces no frames —
 * that is the whole point of a wake-driven framer — so a client that treated a
 * quiet stream as a failure would cry wolf every time the human stopped typing,
 * and one that treated it as health would show a still picture of a magmux that
 * died ten minutes ago. So the client ASKS: a read-class `list` every 2s with a
 * 3s budget, and two consecutive misses is a verdict. Between probes the age of
 * the last frame is shown as information, not as a judgement.
 */
const PROBE_INTERVAL_MS = 2_000;
const PROBE_BUDGET_MS = 3_000;
const PROBE_MISSES_FATAL = 2;
/** How often the status line is refreshed so the frame age keeps moving. */
const STATUS_TICK_MS = 500;
/** A frame older than this is worth saying out loud, without being a failure. */
const STALE_AFTER_MS = 10_000;
const CONNECT_TIMEOUT_MS = 20_000;

// ── the core ────────────────────────────────────────────────────────────────

export class RemoteClient {
  private sock: WebSocket | null = null;
  private screen: Screen = newScreen(0, 0);
  private nextID = 1;
  private probes = new Map<number, number>();
  private misses = 0;
  private lastFrameAt = -1;
  private watching: number | null = null;
  private fps = 0;
  private state: State = "connecting";
  private reason = "";
  private finished = false;
  private settle: ((o: Outcome) => void) | null = null;
  private stops: (() => void)[] = [];
  private helloID = -1;
  private watchID = -1;
  private messages = 0;
  private sawAggregate = false;
  private watchReplyIdx = -1;
  private firstFrameIdx = -1;
  private panes: PaneInfo[] = [];
  private timersOn = false;

  constructor(
    private readonly opts: ClientOptions,
    private readonly painter: Painter,
  ) {}

  private say(msg: string) {
    if (this.opts.verbose) (this.opts.log ?? ((m: string) => console.error(m)))(msg);
  }

  private base(): string {
    return this.opts.url.replace(/\/+$/, "");
  }

  /**
   * Run until the stream ends or fails. Resolves; never rejects.
   */
  run(): Promise<Outcome> {
    return new Promise<Outcome>((resolve) => {
      this.settle = resolve;
      this.start().catch((err) => this.finish(String(err?.message ?? err), false));
    });
  }

  /** Every pane this connection knows about, in id order. */
  knownPanes(): PaneInfo[] {
    return this.panes.slice();
  }

  /** The pane this connection is watching right now, or null before the watch. */
  watchingPane(): number | null {
    return this.watching;
  }

  /**
   * Watch a DIFFERENT pane, over this connection, using nothing but the public
   * API: `unwatch` the old one, `watch` the new one.
   *
   * A watch is per-SUBSCRIBER state — it lives in the hub's slot for this
   * (Sub, pane) pair — and there is deliberately no op that reaches into
   * somebody else's connection to retarget it. So "point the mirror at another
   * pane" cannot be something a third party does TO a client; it has to be
   * something the client does to itself. This method is that act, and the two
   * painters expose it through their own UI (a key in the terminal, a picker in
   * the browser).
   *
   * The reply path does the rest: magmux answers `watch` with the new
   * geometry, the client re-seeds its screen from it, and the first frame for a
   * new watcher is a keyframe by construction — so no row delta is ever applied
   * across the switch.
   *
   * Returns null on success, or the reason it refused.
   */
  retarget(pane: number): string | null {
    if (this.finished) return "this client has already ended";
    if (!this.sock || this.sock.readyState !== 1) return "not connected";
    if (pane === this.watching) return null;
    if (!this.panes.some((p) => p.pane === pane)) {
      return `no pane ${pane} in this connection's pane list`;
    }
    if (this.watching !== null) this.send("unwatch", { pane: this.watching });
    this.watching = pane;
    this.firstFrameIdx = -1;
    this.lastFrameAt = -1;
    this.watchID = this.send("watch", { pane, mode: "frames", fps: this.opts.fps ?? 15 });
    this.say(`retargeting to pane ${pane} (unwatch + watch, on this same connection)`);
    return null;
  }

  /**
   * Move to the next watchable pane, wrapping. `delta` of -1 goes back.
   *
   * The control panel is skipped: it is magmux's own chrome rather than a
   * session, it has no process, and a viewer who cycled into it would see a
   * pane that never changes and conclude the mirror had died.
   */
  cycle(delta: number): string | null {
    const list = this.panes.filter((p) => p.state !== "panel");
    if (!list.length) return "no watchable pane";
    const here = list.findIndex((p) => p.pane === this.watching);
    const next = list[(((here < 0 ? 0 : here + delta) % list.length) + list.length) % list.length];
    return this.retarget(next.pane);
  }

  /** Leave cleanly: unwatch, then close. */
  quit(reason = "quit") {
    if (this.sock && this.sock.readyState === 1 && this.watching !== null) {
      this.send("unwatch", { pane: this.watching });
    }
    this.finish(reason, true);
  }

  private async start() {
    await this.capabilities();
    this.connect();
  }

  /**
   * GET /v1/capabilities BEFORE any WebSocket.
   *
   * An auth failure over a WebSocket is an opaque socket error a browser cannot
   * read reliably — magmux decides every refusal as an HTTP STATUS before any
   * upgrade precisely so a client can learn why. Asking here first turns "the
   * connection failed" into "unauthorized: the token file is stale", and costs
   * one round trip. BOTH the code and the message are reported: `StatusFor`
   * maps five codes onto 409, so the status alone cannot tell a caller which
   * failure it hit.
   */
  private async capabilities(): Promise<void> {
    const ctl = new AbortController();
    const timer = setTimeout(() => ctl.abort(), CONNECT_TIMEOUT_MS);
    let res: Response;
    try {
      res = await fetch(`${this.base()}/v1/capabilities`, {
        headers: { Authorization: `Bearer ${this.opts.token}` },
        signal: ctl.signal,
      });
    } catch (err: any) {
      throw new Error(`cannot reach magmux at ${this.base()}: ${err?.message ?? err}`);
    } finally {
      clearTimeout(timer);
    }
    const text = await res.text();
    let body: any = null;
    try {
      body = JSON.parse(text);
    } catch {}
    if (!res.ok) {
      const code = body?.code ?? "?";
      const msg = body?.error ?? body?.message ?? text.slice(0, 200);
      throw new Error(`magmux refused the credential: HTTP ${res.status} ${code} — ${msg}`);
    }
    this.say(
      `capabilities ok: protocol=${body?.protocol} readOnly=${body?.readOnly} ` +
        `ops=${Array.isArray(body?.ops) ? body.ops.length : "?"}`,
    );
  }

  private connect() {
    const wsURL = `${this.base().replace(/^http/, "ws")}/v1/ws`;
    // The subprotocol is the browser's ONLY credential channel: a browser
    // cannot set an Authorization header on a WebSocket, and a token in the
    // URL lands in every proxy log on the way. magmux echoes `magmux.v1` and
    // never the auth entry.
    const sock = new WebSocket(wsURL, ["magmux.v1", `magmux.auth.${this.opts.token}`]);
    this.sock = sock;

    const opened = setTimeout(() => {
      if (sock.readyState !== 1) this.finish(`the WebSocket never opened within ${CONNECT_TIMEOUT_MS}ms`, false);
    }, CONNECT_TIMEOUT_MS);
    this.stops.push(() => clearTimeout(opened));

    sock.addEventListener("open", () => {
      clearTimeout(opened);
      this.say(`ws open: ${wsURL} (subprotocol ${sock.protocol || "none"})`);
      this.pushStatus();
    });
    sock.addEventListener("message", (e: MessageEvent) => {
      if (typeof e.data !== "string") return;
      this.onMessage(e.data);
    });
    sock.addEventListener("error", () => {
      // A browser deliberately gives no detail here; the close event that
      // follows carries the code, and that is what gets reported.
      this.say("ws error event (no detail is available to a client by design)");
    });
    sock.addEventListener("close", (e: CloseEvent) => {
      if (this.finished) return;
      const why = e.reason ? `${e.reason} (code ${e.code})` : `the server closed the connection (code ${e.code})`;
      // A clean 1000 after `shutdown` is an orderly end; anything else is a
      // failure, because the stream stopping is exactly what must not pass
      // silently for a live screen.
      this.finish(why, false);
    });
  }

  private send(op: string, args?: unknown): number {
    const id = this.nextID++;
    this.sock?.send(JSON.stringify({ id, op, args }));
    return id;
  }

  private onMessage(raw: string) {
    let ev: any;
    try {
      ev = JSON.parse(raw);
    } catch {
      return;
    }
    const idx = this.messages++;

    switch (ev.type) {
      case "snapshot":
        // `snapshot` comes in two shapes: the aggregate, which carries `panes`
        // and is the first line on every connection, and the per-pane one,
        // which carries `pane` and arrives whenever that pane's state changes.
        // Both are used — the first to seed the pane list, the second to keep a
        // label and a state current without asking for anything.
        if (Array.isArray(ev.panes)) {
          this.notePanes(ev.panes);
          if (!this.sawAggregate) {
            this.sawAggregate = true;
            this.say(`#${idx} aggregate: ${ev.panes.length} panes`);
            this.onAggregate(ev);
          }
          this.pushStatus();
        } else if (typeof ev.pane === "number") {
          this.notePanes([ev]);
          this.pushStatus();
        }
        return;
      case "pane_opened":
        if (typeof ev.pane === "number") {
          this.notePanes([{ pane: ev.pane, label: ev.label ?? ev.cmd ?? "", state: "running" }]);
          this.say(`#${idx} pane_opened: ${ev.pane} ${ev.label ?? ev.cmd ?? ""}`);
          this.pushStatus();
        }
        return;
      case "reply":
        this.onReply(idx, ev);
        return;
      case "frame":
        if (ev.pane !== this.watching) return;
        if (this.firstFrameIdx < 0) {
          this.firstFrameIdx = idx;
          this.say(`#${idx} first frame (watch reply was #${this.watchReplyIdx})`);
        }
        this.onFrame(ev as FrameMessage);
        return;
      case "pane_closed":
        this.panes = this.panes.filter((p) => p.pane !== ev.pane);
        if (ev.pane === this.watching) this.finish(`pane ${ev.pane} closed`, true);
        else this.pushStatus();
        return;
      case "results":
        this.say(`#${idx} results — magmux is shutting down`);
        return;
      case "shutdown":
        this.finish("magmux shut down", true);
        return;
    }
  }

  /**
   * The aggregate is the first message on every transport and is where a client
   * seeds its pane map. Picking the pane from it rather than guessing 0 matters
   * because every session now holds a control panel and pane ids are sparse:
   * "the first pane" is not "pane 0".
   */
  /**
   * Merge pane records in, by id, without ever losing a field to an update
   * that did not carry it.
   *
   * A per-pane `snapshot` may say nothing about the label, and `pane_opened`
   * says nothing about the state that the next snapshot will. Overwriting
   * wholesale would make a picker's labels flicker in and out, which reads as a
   * bug in the mirror.
   */
  private notePanes(list: any[]) {
    for (const p of list) {
      if (typeof p?.pane !== "number") continue;
      const found = this.panes.find((q) => q.pane === p.pane);
      const label = String(p.label ?? found?.label ?? "");
      const state = String(p.state ?? found?.state ?? "");
      if (found) {
        found.label = label;
        found.state = state;
      } else {
        this.panes.push({ pane: p.pane, label, state });
      }
    }
    this.panes.sort((a, b) => a.pane - b.pane);
  }

  private onAggregate(ev: any) {
    let pane = this.opts.pane ?? null;
    if (pane === null) {
      for (const p of ev.panes ?? []) {
        if (p?.state === "panel") continue;
        if (typeof p?.pane === "number") {
          pane = p.pane;
          break;
        }
      }
    }
    if (pane === null) {
      this.finish("the aggregate names no session pane to watch", false);
      return;
    }
    this.watching = pane;
    this.send("hello", { client: this.opts.client ?? "magmux-rc-demo" });
    this.helloID = this.nextID - 1;
    this.watchID = this.send("watch", { pane, mode: "frames", fps: this.opts.fps ?? 15 });
  }

  private onReply(idx: number, ev: any) {
    const id = Number(ev.id);
    if (this.probes.has(id)) {
      this.probes.delete(id);
      this.misses = 0;
      return;
    }
    if (id === this.helloID) {
      if (ev.ok !== true) return this.finish(`hello failed: ${ev.code} — ${ev.error}`, false);
      this.say(`#${idx} hello: readOnly=${ev.result?.readOnly} protocol=${ev.result?.protocol}`);
      return;
    }
    if (id === this.watchID) {
      if (ev.ok !== true) return this.finish(`watch failed: ${ev.code} — ${ev.error}`, false);
      const r = ev.result ?? {};
      this.watchReplyIdx = idx;
      this.fps = r.fps ?? 0;
      this.screen = newScreen(r.rows ?? 0, r.cols ?? 0, r.pane ?? 0);
      this.say(`#${idx} watch reply: pane=${r.pane} ${r.rows}x${r.cols} mode=${r.mode} fps=${r.fps}`);
      this.painter.size(r.rows ?? 0, r.cols ?? 0);
      this.state = "live";
      this.pushStatus();
      this.startTimers();
      return;
    }
    if (ev.ok !== true) this.say(`#${idx} reply ${id} failed: ${ev.code} — ${ev.error}`);
  }

  /**
   * Geometry is re-read from EVERY frame header, not taken once from the watch
   * reply.
   *
   * A pane reflows whenever the human resizes the window, and in a demo that is
   * the most likely thing anyone does. magmux forces a keyframe on a resize and
   * on an alt-screen switch — a client cannot apply row deltas across either —
   * so the sequence here is: `size()`, then `clear()`, then every row. That
   * makes `Painter.size()` reachable from the frame path and therefore
   * IDEMPOTENT by contract: both painters must tolerate being told the size
   * they already have.
   */
  private onFrame(frame: FrameMessage) {
    const wasRows = this.screen.rows;
    const wasCols = this.screen.cols;
    const wasAlt = this.screen.alt;
    const changed = applyFrame(this.screen, frame);
    if (this.screen.rows !== wasRows || this.screen.cols !== wasCols) {
      this.say(`geometry changed: ${wasCols}x${wasRows} -> ${this.screen.cols}x${this.screen.rows}`);
    }
    if (this.screen.alt !== wasAlt) {
      this.say(`alt screen ${this.screen.alt ? "entered" : "left"}`);
    }
    if (this.screen.cleared) {
      this.painter.size(this.screen.rows, this.screen.cols);
      this.painter.clear();
    }
    for (const y of changed) {
      const line = this.screen.lines[y];
      this.painter.row(y, line, cellCols(line));
    }
    // Always, even when no row changed: a moved or hidden cursor is a frame in
    // its own right, and a client that skipped it loses the caret.
    this.painter.cursor(this.screen.cur);
    this.lastFrameAt = Date.now();
    if (this.state === "stale") this.state = "live";
    this.pushStatus();
  }

  /**
   * Once, and only once. `retarget` produces a SECOND `watch` reply on the same
   * connection, and a second set of timers would double the liveness probe rate
   * and leave the first set running with nothing to stop it.
   */
  private startTimers() {
    if (this.timersOn) return;
    this.timersOn = true;
    const probe = setInterval(() => this.tickProbe(), PROBE_INTERVAL_MS);
    const status = setInterval(() => {
      if (this.state === "live" && this.ageMs() > STALE_AFTER_MS) this.state = "stale";
      this.pushStatus();
    }, STATUS_TICK_MS);
    this.stops.push(() => clearInterval(probe), () => clearInterval(status));
  }

  private tickProbe() {
    if (this.finished || !this.sock || this.sock.readyState !== 1) return;
    const now = Date.now();
    for (const [id, at] of this.probes) {
      if (now - at > PROBE_BUDGET_MS) {
        this.probes.delete(id);
        this.misses++;
      }
    }
    if (this.misses >= PROBE_MISSES_FATAL) {
      this.finish(
        `magmux stopped answering: ${this.misses} liveness probes went unanswered ` +
          `within ${PROBE_BUDGET_MS}ms each`,
        false,
      );
      return;
    }
    this.probes.set(this.send("list"), now);
  }

  private ageMs(): number {
    return this.lastFrameAt < 0 ? -1 : Date.now() - this.lastFrameAt;
  }

  private pushStatus() {
    this.painter.status({
      state: this.state,
      reason: this.reason,
      pane: this.watching ?? -1,
      seq: this.screen.seq,
      frames: this.screen.frames,
      fps: this.fps,
      ageMs: this.ageMs(),
      rows: this.screen.rows,
      cols: this.screen.cols,
      scrolled: this.screen.scrolled,
      alt: this.screen.alt,
      panes: this.panes.slice(),
    });
  }

  private finish(reason: string, ok: boolean) {
    if (this.finished) return;
    this.finished = true;
    this.reason = reason;
    this.state = ok ? "ended" : "dead";
    for (const stop of this.stops) stop();
    this.stops = [];
    this.pushStatus();
    try {
      this.sock?.close();
    } catch {}
    this.painter.end(reason, ok);
    this.settle?.({ ok, reason });
    this.settle = null;
  }
}
