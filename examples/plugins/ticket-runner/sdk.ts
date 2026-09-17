/**
 * A minimal magmux plugin SDK for bun/node.
 *
 * A plugin is an ordinary socket client that also registers. Everything here is
 * therefore one connection: the plugin protocol (`plugin.register`, `invoke`,
 * `invoke_result`, `plugin.event`, `controller.snapshot`) and the ordinary
 * verbs a plugin uses to drive its panes (`open_pane`, `send`, `watch`) share
 * it, because identity lives on the connection — a pane claimed with
 * `{"controller":"self"}` is claimed by whoever registered on THAT socket.
 *
 * The line framing and the request/reply plumbing are the same shapes
 * pilot/magmux.ts uses; what is new is the inbound direction, where magmux
 * drives the plugin instead of the other way round.
 */
import net from "node:net";

export type OpClass = "read" | "control" | "display" | "input";

export interface PluginOp {
  /** Bare name, [a-z][a-z0-9_]{0,29}. magmux advertises it as `<plugin>.<name>`. */
  name: string;
  description: string;
  /** A capability claim, not a label: `read` is what a view-only caller may reach. */
  class: OpClass;
  /** JSON Schema object for the args. */
  schema?: Record<string, unknown>;
  handler: (req: Invocation) => Promise<Record<string, unknown>>;
}

export interface Invocation {
  op: string;
  args: Record<string, unknown>;
  caller: { transport?: string; conn?: string; client?: string; readOnly?: boolean };
  /** Fires when magmux stops waiting: the caller's budget expired, or it is shutting down. */
  signal: AbortSignal;
}

export interface ControllerSnapshot {
  pane: number;
  state: "starting" | "working" | "awaiting_input" | "awaiting_permission" | "error" | "gone";
  response?: string;
  tool?: string;
  prompt?: string;
  model?: string;
  project?: string;
  error?: string;
}

/** A failure with one of magmux's own codes, so a caller can branch on it. */
export class OpError extends Error {
  constructor(readonly code: string, message: string) {
    super(message);
  }
}

interface Pending {
  resolve: (v: Record<string, unknown>) => void;
  reject: (e: Error) => void;
  timer: ReturnType<typeof setTimeout>;
}

export class MagmuxPlugin {
  private conn: net.Socket | null = null;
  private buf = "";
  private nextId = 1;
  private pending = new Map<string, Pending>();
  private running = new Map<string, AbortController>();
  private closed = false;
  private closedWaiters: Array<() => void> = [];

  private readonly ops = new Map<string, PluginOp>();
  private readonly sock: string;
  private readonly token: string;

  /** Frame watchers, by pane. See watch(). */
  private readonly views = new Map<number, PaneView>();

  constructor(
    private readonly opts: {
      name: string;
      version?: string;
      ops: PluginOp[];
      events?: string[];
      sock?: string;
      token?: string;
    },
  ) {
    for (const op of opts.ops) this.ops.set(op.name, op);
    const sock = opts.sock ?? process.env.MAGMUX_SOCK ?? "";
    if (!sock) throw new Error("no magmux socket: MAGMUX_SOCK is unset");
    this.sock = sock;
    // MAGMUX_PLUGIN_TOKEN for a plugin magmux started; MAGMUX_TOKEN for one a
    // developer is running by hand against a live session.
    const token = opts.token ?? process.env.MAGMUX_PLUGIN_TOKEN ?? process.env.MAGMUX_TOKEN ?? "";
    if (!token) throw new Error("no token: magmux sets MAGMUX_PLUGIN_TOKEN for plugins it starts");
    this.token = token;
  }

  /** Dial magmux and register. Invocations can arrive the moment it resolves. */
  async connect(): Promise<void> {
    await new Promise<void>((resolve, reject) => {
      const conn = net.createConnection(this.sock);
      this.conn = conn;
      conn.once("connect", () => resolve());
      conn.once("error", (e) => {
        if (!this.closed) reject(e);
        this.markClosed();
      });
      conn.on("end", () => this.markClosed());
      conn.on("close", () => this.markClosed());
      conn.on("data", (chunk) => this.ingest(chunk.toString()));
    });

    await this.request({
      type: "plugin.register",
      token: this.token,
      name: this.opts.name,
      version: this.opts.version ?? "0.0.0",
      ops: this.opts.ops.map((op) => ({
        name: op.name,
        description: op.description,
        class: op.class,
        schema: op.schema ?? { type: "object" },
      })),
      events: this.opts.events ?? [],
    });
  }

  /** Resolves when magmux closes the connection — its teardown, or ours. */
  wait(): Promise<void> {
    if (this.closed) return Promise.resolve();
    return new Promise((resolve) => this.closedWaiters.push(resolve));
  }

  close() {
    this.markClosed();
    try {
      this.conn?.destroy();
    } catch {}
  }

  // ── outbound ──────────────────────────────────────────────────────────────

  /** Send a message and wait for its single reply. */
  request(msg: Record<string, unknown>, timeoutMs = 30_000): Promise<Record<string, unknown>> {
    const id = String(this.nextId++);
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => {
        this.pending.delete(id);
        reject(new OpError("timeout", `magmux did not answer ${String(msg.type)} within ${timeoutMs}ms`));
      }, timeoutMs);
      this.pending.set(id, { resolve, reject, timer });
      this.write({ ...msg, id });
    });
  }

  /** Send a message that magmux answers with nothing. */
  fire(msg: Record<string, unknown>) {
    this.write(msg);
  }

  /** Publish one event to every subscriber. It must be one the plugin declared. */
  event(event: string, pane: number | undefined, data: Record<string, unknown>) {
    const msg: Record<string, unknown> = { type: "plugin.event", event, data };
    if (pane !== undefined) msg.pane = pane;
    this.fire(msg);
  }

  /** Report what the tool in one of this plugin's panes is doing. */
  async pushSnapshot(snap: ControllerSnapshot): Promise<void> {
    await this.request({ type: "controller.snapshot", ...snap });
  }

  /** Open a pane. Pass controller:"self" to make this plugin its observer. */
  async openPane(req: Record<string, unknown>): Promise<number> {
    const res = await this.request({ type: "open_pane", ...req });
    return Number(res.pane);
  }

  async closePane(pane: number, force = false): Promise<void> {
    await this.request({ type: "close_pane", pane, force });
  }

  /** Deliver an instruction to a pane: text, then Enter unless told otherwise. */
  async send(pane: number, text: string, opts: { label?: string; enter?: boolean } = {}): Promise<void> {
    await this.request(
      { type: "send", pane, text, label: opts.label, enter: opts.enter ?? true },
      60_000,
    );
  }

  async capture(pane: number, lines = 0): Promise<string> {
    const res = await this.request({ type: "capture", pane, lines });
    return String(res.text ?? "");
  }

  /**
   * Watch a pane's screen and keep a live copy of it.
   *
   * Frames are whole ROWS, so applying one is replacing the rows it names —
   * that is what makes a dropped frame harmless and why this needs no
   * acknowledgement of any kind.
   */
  async watch(pane: number, fps = 5): Promise<PaneView> {
    let view = this.views.get(pane);
    if (!view) {
      view = new PaneView();
      this.views.set(pane, view);
    }
    await this.request({ type: "watch", pane, mode: "frames", fps });
    return view;
  }

  async unwatch(pane: number): Promise<void> {
    this.views.delete(pane);
    try {
      await this.request({ type: "unwatch", pane });
    } catch {
      // A pane that closed under us is already unwatched; nothing to repair.
    }
  }

  private write(msg: Record<string, unknown>) {
    if (!this.conn || this.closed) return;
    this.conn.write(JSON.stringify(msg) + "\n");
  }

  // ── inbound ───────────────────────────────────────────────────────────────

  private ingest(chunk: string) {
    this.buf += chunk;
    let nl: number;
    while ((nl = this.buf.indexOf("\n")) >= 0) {
      const line = this.buf.slice(0, nl).trim();
      this.buf = this.buf.slice(nl + 1);
      if (!line) continue;
      let ev: Record<string, any>;
      try {
        ev = JSON.parse(line);
      } catch {
        continue;
      }
      switch (ev.type) {
        case "reply":
          this.routeReply(ev);
          break;
        case "invoke":
          this.dispatch(ev);
          break;
        case "invoke_cancel":
          this.running.get(String(ev.call))?.abort();
          break;
        case "frame":
          this.views.get(Number(ev.pane))?.apply(ev);
          break;
        case "shutdown":
          // magmux is going away; the EOF is next. Nothing to do but let wait()
          // resolve when the socket closes.
          break;
      }
    }
  }

  private routeReply(ev: Record<string, any>) {
    const p = this.pending.get(String(ev.id));
    if (!p) return;
    this.pending.delete(String(ev.id));
    clearTimeout(p.timer);
    if (ev.ok) p.resolve((ev.result ?? {}) as Record<string, unknown>);
    else p.reject(new OpError(String(ev.code ?? "internal"), String(ev.error ?? "the request failed")));
  }

  /**
   * Run one invocation.
   *
   * Never inline: this is the reader, and a handler that blocked it could
   * neither receive its own cancellation nor get an answer to any request it
   * made — which is most of what a handler does.
   */
  private dispatch(ev: Record<string, any>) {
    const call = String(ev.call);
    const op = this.ops.get(String(ev.op));
    if (!op) {
      this.answer(call, false, undefined, "unknown_verb", `no op ${ev.op}`);
      return;
    }
    const ac = new AbortController();
    this.running.set(call, ac);
    const done = (ok: boolean, result?: Record<string, unknown>, code?: string, error?: string) => {
      this.running.delete(call);
      this.answer(call, ok, result, code, error);
    };
    op.handler({
      op: String(ev.op),
      args: (ev.args ?? {}) as Record<string, unknown>,
      caller: ev.caller ?? {},
      signal: ac.signal,
    }).then(
      (result) => done(true, result),
      (err: unknown) => {
        const code = err instanceof OpError ? err.code : "internal";
        done(false, undefined, code, err instanceof Error ? err.message : String(err));
      },
    );
  }

  private answer(
    call: string,
    ok: boolean,
    result?: Record<string, unknown>,
    code?: string,
    error?: string,
  ) {
    const msg: Record<string, unknown> = { type: "invoke_result", call, ok };
    if (ok) {
      if (result) msg.result = result;
    } else {
      msg.code = code ?? "internal";
      msg.error = error ?? "the op failed";
    }
    this.fire(msg);
  }

  private markClosed() {
    if (this.closed) return;
    this.closed = true;
    for (const [, p] of this.pending) {
      clearTimeout(p.timer);
      p.reject(new OpError("not_ready", "magmux disconnected"));
    }
    this.pending.clear();
    for (const [, ac] of this.running) ac.abort();
    this.running.clear();
    const waiters = this.closedWaiters;
    this.closedWaiters = [];
    for (const w of waiters) w();
  }
}

/**
 * A pane's screen, rebuilt from frames.
 *
 * A keyframe carries every row and the client clears first; a delta carries
 * only the rows that changed, each replacing its whole row. So applying frames
 * in order needs no state beyond the rows themselves, and missing one only ever
 * costs the rows that frame carried.
 */
export class PaneView {
  rows: string[] = [];
  seq = 0;
  private waiters: Array<() => void> = [];

  apply(frame: Record<string, any>) {
    if (frame.key) this.rows = new Array<string>(Number(frame.rows ?? 0)).fill("");
    for (const line of (frame.lines ?? []) as Array<Record<string, any>>) {
      const y = Number(line.y);
      while (this.rows.length <= y) this.rows.push("");
      this.rows[y] = String(line.t ?? "");
    }
    this.seq = Number(frame.seq ?? this.seq);
    const waiters = this.waiters;
    this.waiters = [];
    for (const w of waiters) w();
  }

  text(): string {
    return this.rows.join("\n");
  }

  /** Wait until the screen matches pred, or give up after timeoutMs. */
  waitFor(pred: (text: string) => boolean, timeoutMs: number): Promise<boolean> {
    if (pred(this.text())) return Promise.resolve(true);
    return new Promise((resolve) => {
      const timer = setTimeout(() => {
        this.waiters = this.waiters.filter((w) => w !== onChange);
        resolve(false);
      }, timeoutMs);
      const onChange = () => {
        if (!pred(this.text())) {
          this.waiters.push(onChange);
          return;
        }
        clearTimeout(timer);
        resolve(true);
      };
      this.waiters.push(onChange);
    });
  }
}
