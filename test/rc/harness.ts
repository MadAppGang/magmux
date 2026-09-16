/**
 * Shared harness for the remote-control end-to-end cases.
 *
 * These are the six validation criteria, run against a REAL magmux binary over
 * the REAL transports — HTTP, WebSocket, server-sent events, the unix socket,
 * `magmux mcp` on stdio, and a Firebase Realtime Database emulator. Nothing
 * here is mocked; the Go suite already proves every piece in isolation, and the
 * only thing these cases can add is that the pieces fit when they are separate
 * processes talking over sockets.
 *
 * The readiness contract is the one `mux/remote.go` documents and every case
 * leans on: the tokens are resolved and written, the TLS pair is loaded, and
 * only THEN does the listener bind. So an accepting port implies a final token
 * file at its final name, and `magmux: listening on <url>` on stderr is the
 * signal to read it. No case sleeps waiting for startup.
 */
import { execFileSync, spawn, type ChildProcess } from "node:child_process";
import crypto from "node:crypto";
import fs from "node:fs";
import net from "node:net";
import os from "node:os";
import path from "node:path";

export { C, header, report, sleep, truncate, type Check } from "../ui/harness.ts";
import { C, sleep } from "../ui/harness.ts";

export const REPO = path.resolve(import.meta.dir, "../..");
export const BIN = path.join(REPO, "magmux");
export const TICKET_PLUGIN = path.join(REPO, "examples/plugins/ticket-runner/main.ts");

/**
 * Build the binary under test.
 *
 * Always a real build, never the checked-in `./magmux`: `magmux mcp`, the
 * multiplexer and the HTTP server are ONE binary, so a stale artifact would
 * test the previous version of the very thing under test. MAGMUX_BIN points a
 * case at an arbitrary build instead — which is how you confirm a case catches
 * the regression it claims to.
 */
export function buildMagmux(): string {
  const override = process.env.MAGMUX_BIN;
  if (override) return path.resolve(override);
  execFileSync("go", ["build", "-o", BIN, "./cmd/magmux"], { cwd: REPO, stdio: "inherit" });
  return BIN;
}

/**
 * A token in magmux's own alphabet: 32 random bytes as unpadded base64url,
 * which is 43 characters of [A-Za-z0-9._~-]. The same shape `auth.Generate`
 * produces, because `auth.LoadFile` refuses anything else.
 */
export function newToken(): string {
  return crypto.randomBytes(32).toString("base64url");
}

/** Write a token file the way magmux insists on finding one: regular, 0600. */
export function writeTokenFile(file: string, token: string) {
  fs.writeFileSync(file, token + "\n", { mode: 0o600 });
  fs.chmodSync(file, 0o600);
}

/** A private directory for one case's socket, tokens and config. */
export function scratchDir(tag: string): string {
  // /tmp rather than os.tmpdir(): on darwin TMPDIR is long enough that a unix
  // socket under it can overrun sun_path, and magmux binds one.
  const dir = fs.mkdtempSync(path.join("/tmp", `mmrc-${tag}-`));
  return dir;
}

export interface HTTPResult {
  status: number;
  text: string;
  json: any;
  headers: Headers;
}

/** One magmux process with remote control on, and everything a case needs to reach it. */
export class RemoteMagmux {
  proc: ChildProcess | null = null;
  base = "";
  addr = "";
  token = "";
  viewToken = "";
  dir: string;
  sockDir: string;
  sock = "";
  /** The `magmux: mirroring to <url>` line, when --firebase was given. */
  mirrorURL = "";
  private stderrBuf = "";
  private exited = false;
  private exitCode: number | null = null;

  constructor(readonly bin: string, readonly tag: string) {
    this.dir = scratchDir(tag);
    this.sockDir = this.dir;
  }

  get stderr(): string {
    return this.stderrBuf;
  }

  get hasExited(): boolean {
    return this.exited;
  }

  /**
   * Start magmux headless, with a listener on an ephemeral port, and wait until
   * it announces the URL.
   *
   * `--headless` and a /dev/null stdin are both deliberate: a case is not
   * watching a screen, and a pipe stdin would make magmux degrade on its own
   * anyway. `--sock-dir` keeps the case's socket out of the operator's /tmp, so
   * a case can never attach to a magmux somebody else is using.
   */
  async start(args: string[], opts: { viewToken?: boolean; env?: NodeJS.ProcessEnv } = {}) {
    const full = ["--headless", "--listen", "127.0.0.1:0", "--sock-dir", this.sockDir];
    if (opts.viewToken) {
      this.viewToken = newToken();
      const f = path.join(this.dir, "view.token");
      writeTokenFile(f, this.viewToken);
      full.push("--view-token-file", f);
    }
    full.push(...args);

    const env = { ...process.env, ...(opts.env ?? {}) };
    // A magmux started from inside a magmux inherits its parent's geometry
    // through COLUMNS/LINES. Pin them so every case sees the same grid.
    env.COLUMNS = env.COLUMNS ?? "100";
    env.LINES = env.LINES ?? "30";
    this.proc = spawn(this.bin, full, { cwd: REPO, env, stdio: ["ignore", "pipe", "pipe"] });
    this.proc.stdout?.on("data", () => {});
    this.proc.stderr?.on("data", (b) => {
      this.stderrBuf += b.toString();
    });
    this.proc.on("exit", (code) => {
      this.exited = true;
      this.exitCode = code;
    });
    this.sock = path.join(this.sockDir, `magmux-${this.proc.pid}.sock`);

    await this.waitForLine(/listening on (https?:\/\/[^\s]+)/, 30_000, (m) => {
      this.base = m[1];
      this.addr = this.base.replace(/^https?:\/\//, "");
    });

    // The token is read OFF DISK, never handed over, because that is exactly
    // what an operator does and it is the property worth testing: by the time
    // the URL was printed the file is at its final name with its final bytes.
    const m = /token in (\S+)/.exec(this.stderrBuf);
    if (m) this.token = fs.readFileSync(m[1], "utf8").trim();
    if (!this.token) throw new Error(`magmux never named a token file:\n${this.stderrBuf}`);
  }

  /** Wait for a stderr line, failing fast if magmux dies first. */
  async waitForLine(re: RegExp, timeoutMs: number, onMatch?: (m: RegExpExecArray) => void): Promise<RegExpExecArray> {
    const deadline = Date.now() + timeoutMs;
    while (Date.now() < deadline) {
      const m = re.exec(this.stderrBuf);
      if (m) {
        onMatch?.(m);
        return m;
      }
      if (this.exited) {
        throw new Error(`magmux exited (${this.exitCode}) before matching ${re}\n${this.stderrBuf}`);
      }
      await sleep(20);
    }
    throw new Error(`magmux never printed ${re} within ${timeoutMs}ms\n${this.stderrBuf}`);
  }

  /** One REST call. `token: null` sends no credential at all — the 401 case. */
  async http(
    method: string,
    p: string,
    opts: { token?: string | null; body?: unknown; headers?: Record<string, string> } = {},
  ): Promise<HTTPResult> {
    const headers: Record<string, string> = { ...(opts.headers ?? {}) };
    const tok = opts.token === undefined ? this.token : opts.token;
    if (tok) headers.Authorization = `Bearer ${tok}`;
    if (opts.body !== undefined) headers["Content-Type"] = "application/json";
    const res = await fetch(this.base + p, {
      method,
      headers,
      body: opts.body === undefined ? undefined : JSON.stringify(opts.body),
    });
    const text = await res.text();
    let json: any = null;
    try {
      json = JSON.parse(text);
    } catch {}
    return { status: res.status, text, json, headers: res.headers };
  }

  /**
   * Poll a REST call until `ok` says it is what the case was waiting for.
   *
   * Every wait in this suite is a poll with a generous cap, never a sleep: a
   * case that sleeps long enough for an idle laptop is a case that fails on a
   * loaded one, and a case that sleeps long enough for a loaded one wastes the
   * difference on every run.
   */
  async httpUntil(
    method: string,
    p: string,
    ok: (r: HTTPResult) => boolean,
    within = 20_000,
    opts: Parameters<RemoteMagmux["http"]>[2] = {},
  ): Promise<HTTPResult> {
    const deadline = Date.now() + within;
    let last: HTTPResult | null = null;
    while (Date.now() < deadline) {
      last = await this.http(method, p, opts);
      if (ok(last)) return last;
      await sleep(50);
    }
    throw new Error(`${method} ${p} never satisfied the wait: ${last?.status} ${last?.text}`);
  }

  /** A WebSocket on /v1/ws, credentialled through the subprotocol channel. */
  ws(token?: string): WSClient {
    const tok = token ?? this.token;
    return new WSClient(`${this.base.replace(/^http/, "ws")}/v1/ws`, tok);
  }

  /** The unix socket, as any pre-HTTP client reaches it. */
  dialSocket(): Promise<net.Socket> {
    return new Promise((resolve, reject) => {
      const c = net.createConnection(this.sock);
      c.once("connect", () => resolve(c));
      c.once("error", reject);
    });
  }

  async stop() {
    if (!this.proc || this.exited) return;
    try {
      this.proc.kill("SIGTERM");
    } catch {}
    for (let i = 0; i < 100 && !this.exited; i++) await sleep(50);
    if (!this.exited) {
      try {
        this.proc.kill("SIGKILL");
      } catch {}
    }
  }

  cleanup() {
    try {
      fs.rmSync(this.dir, { recursive: true, force: true });
    } catch {}
  }
}

/**
 * A WebSocket client that records every message with the time it arrived.
 *
 * The token goes in Sec-WebSocket-Protocol as `magmux.auth.<token>`, which is
 * the browser channel: a browser cannot set an Authorization header on a
 * WebSocket, and this suite uses the path a browser would so that the path a
 * browser would use is the one under test.
 */
export class WSClient {
  readonly messages: { at: number; ev: any; raw: string }[] = [];
  private sock: WebSocket | null = null;
  private waiters: { pred: (ev: any, raw: string) => boolean; resolve: (v: any) => void; timer: any }[] = [];
  closed = false;
  closeCode = 0;
  closeReason = "";
  private nextID = 1;

  constructor(
    readonly url: string,
    readonly token: string,
  ) {}

  async open(timeoutMs = 20_000): Promise<void> {
    const protocols = ["magmux.v1", `magmux.auth.${this.token}`];
    const sock = new WebSocket(this.url, protocols);
    this.sock = sock;
    sock.addEventListener("message", (e: MessageEvent) => {
      const raw = typeof e.data === "string" ? e.data : "";
      let ev: any = null;
      try {
        ev = JSON.parse(raw);
      } catch {
        return;
      }
      const rec = { at: Date.now(), ev, raw };
      this.messages.push(rec);
      for (let i = this.waiters.length - 1; i >= 0; i--) {
        const w = this.waiters[i];
        if (w.pred(ev, raw)) {
          this.waiters.splice(i, 1);
          clearTimeout(w.timer);
          w.resolve(rec);
        }
      }
    });
    sock.addEventListener("close", (e: CloseEvent) => {
      this.closed = true;
      this.closeCode = e.code;
      this.closeReason = e.reason;
    });
    await new Promise<void>((resolve, reject) => {
      const timer = setTimeout(() => reject(new Error(`websocket never opened within ${timeoutMs}ms`)), timeoutMs);
      sock.addEventListener("open", () => {
        clearTimeout(timer);
        resolve();
      });
      sock.addEventListener("error", () => {
        clearTimeout(timer);
        reject(new Error("websocket handshake failed"));
      });
    });
  }

  send(op: string, args?: unknown): number {
    const id = this.nextID++;
    this.sock!.send(JSON.stringify({ id, op, args }));
    return id;
  }

  /** Wait for a message, from NOW — messages already recorded do not count. */
  await(pred: (ev: any, raw: string) => boolean, timeoutMs: number, what: string): Promise<{ at: number; ev: any; raw: string }> {
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => {
        const i = this.waiters.findIndex((w) => w.timer === timer);
        if (i >= 0) this.waiters.splice(i, 1);
        reject(new Error(`never saw ${what} within ${timeoutMs}ms (${this.messages.length} messages, closed=${this.closed} code=${this.closeCode})`));
      }, timeoutMs);
      this.waiters.push({ pred, resolve, timer });
    });
  }

  /** As `await`, but a message already in the log satisfies it. */
  async seen(pred: (ev: any, raw: string) => boolean, timeoutMs: number, what: string) {
    for (const m of this.messages) if (pred(m.ev, m.raw)) return m;
    return this.await(pred, timeoutMs, what);
  }

  async reply(id: number, timeoutMs = 30_000) {
    const m = await this.seen((ev) => ev.type === "reply" && String(ev.id) === String(id), timeoutMs, `reply to id ${id}`);
    if (m.ev.ok !== true) throw new Error(`reply to ${id} failed: ${m.raw}`);
    return m.ev.result ?? {};
  }

  async call(op: string, args?: unknown, timeoutMs = 30_000) {
    return this.reply(this.send(op, args), timeoutMs);
  }

  close() {
    try {
      this.sock?.close();
    } catch {}
  }
}

/** Every text row of a frame, joined — what a viewer would see on that pane. */
export function frameText(ev: any): string {
  const lines = Array.isArray(ev?.lines) ? ev.lines : [];
  return lines.map((l: any) => String(l?.t ?? "")).join("\n");
}

/** The first non-panel pane in a connect-time aggregate. */
export function firstSessionPane(agg: any): number {
  for (const p of agg?.panes ?? []) {
    if (p?.state === "panel") continue;
    if (typeof p?.pane === "number") return p.pane;
  }
  throw new Error(`no session pane in the aggregate: ${JSON.stringify(agg)}`);
}

/** Poll a predicate with a generous cap. Never a sleep sized to one machine. */
export async function until(what: string, within: number, cond: () => boolean | Promise<boolean>): Promise<void> {
  const deadline = Date.now() + within;
  while (Date.now() < deadline) {
    if (await cond()) return;
    await sleep(25);
  }
  throw new Error(`timed out after ${within}ms waiting for ${what}`);
}

// ── reporting ───────────────────────────────────────────────────────────────

/** One criterion's result line, in the form run-all.ts parses. */
export function criterion(n: number, name: string, pass: boolean, detail: string) {
  const mark = pass ? `${C.green}PASS${C.reset}` : `${C.red}FAIL${C.reset}`;
  console.log(`\n  ${mark}  ${C.bold}criterion ${n}${C.reset} — ${name}`);
  if (detail) console.log(`        ${C.grey}${detail}${C.reset}`);
  console.log(`__RC__ ${n} ${pass ? "PASS" : "FAIL"} ${name} — ${detail}`);
}

/** Report a criterion nothing could run, and say what would make it run. */
export function skipped(n: number, name: string, why: string) {
  console.log(`\n  ${C.yellow}SKIP${C.reset}  ${C.bold}criterion ${n}${C.reset} — ${name}`);
  console.log(`        ${C.grey}${why}${C.reset}`);
  console.log(`__RC__ ${n} SKIP ${name} — ${why}`);
}

/** A magmux that died is worth one look at its stderr before the case reports. */
export function dumpStderr(m: RemoteMagmux) {
  const tail = m.stderr.split("\n").filter(Boolean).slice(-20);
  if (!tail.length) return;
  console.log(`\n  ${C.bold}magmux stderr (tail)${C.reset}`);
  for (const l of tail) console.log(`  ${C.grey}│${C.reset} ${l.slice(0, 160)}`);
}

export function hostname(): string {
  return os.hostname().replace(/[^A-Za-z0-9_-]/g, "-").slice(0, 32) || "rc-host";
}
