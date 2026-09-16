/**
 * A JSON-RPC client for `magmux mcp` over stdio.
 *
 * It holds the server to the rule the whole MCP surface rests on: the child
 * writes JSON-RPC to stdout and NOTHING else. Every line is parsed as it is
 * read and a line that is not JSON is recorded as a violation, so one stray
 * `fmt.Println` anywhere in the binary fails the case that uses this.
 *
 * Its log is never rewound. `await` scans what has already arrived and then
 * waits, because a notification can legitimately precede the response to the
 * call that caused it — a plugin's `progress` races its own tool result, and
 * asserting one order would be asserting a race.
 */
import { spawn, type ChildProcess } from "node:child_process";
import path from "node:path";
import { sleep } from "./harness.ts";

export class MCPChild {
  private proc: ChildProcess | null = null;
  private buf = "";
  private nextID = 1;
  readonly messages: any[] = [];
  readonly stray: string[] = [];
  stderr = "";
  exited = false;

  constructor(
    readonly bin: string,
    readonly sockDir: string,
  ) {}

  start() {
    this.proc = spawn(this.bin, ["mcp"], {
      env: {
        ...process.env,
        MAGMUX_SOCK_DIR: this.sockDir,
        // MAGMUX_SOCK would attach the child to whatever magmux is running this
        // case's own shell. Discovery in sockDir is the path under test.
        MAGMUX_SOCK: "",
        MAGMUX_MCP_LOG: path.join(this.sockDir, "mcp.log"),
      },
      stdio: ["pipe", "pipe", "pipe"],
    });
    this.proc.stdout?.on("data", (b) => this.feed(b.toString()));
    this.proc.stderr?.on("data", (b) => (this.stderr += b.toString()));
    this.proc.on("exit", () => (this.exited = true));
  }

  private feed(chunk: string) {
    this.buf += chunk;
    let nl: number;
    while ((nl = this.buf.indexOf("\n")) >= 0) {
      const line = this.buf.slice(0, nl).trim();
      this.buf = this.buf.slice(nl + 1);
      if (!line) continue;
      try {
        const msg = JSON.parse(line);
        if (msg && typeof msg === "object" && msg.jsonrpc === "2.0") {
          this.messages.push(msg);
          continue;
        }
        this.stray.push(line);
      } catch {
        this.stray.push(line);
      }
    }
  }

  send(msg: Record<string, unknown>) {
    this.proc!.stdin!.write(JSON.stringify(msg) + "\n");
  }

  /** How many requests this client has issued — one response is owed for each. */
  get requestCount(): number {
    return this.nextID - 1;
  }

  /** The current end of the log, for an `await` that must ignore the past. */
  get mark(): number {
    return this.messages.length;
  }

  request(method: string, params?: unknown): number {
    const id = this.nextID++;
    this.send({ jsonrpc: "2.0", id, method, ...(params === undefined ? {} : { params }) });
    return id;
  }

  /**
   * Wait for a message satisfying `pred`, starting the scan at `from`.
   *
   * `from` is what makes "a notification caused by the thing I just did"
   * expressible. Without it a case takes a mark from the log, does its thing,
   * and then matches a notification that arrived BEFORE the mark — which is the
   * shape of a test that passes against a server that notifies nobody.
   */
  async await(what: string, timeoutMs: number, pred: (m: any) => boolean, from = 0): Promise<any> {
    const deadline = Date.now() + timeoutMs;
    let i = from;
    while (Date.now() < deadline) {
      for (; i < this.messages.length; i++) {
        if (pred(this.messages[i])) return this.messages[i];
      }
      if (this.exited) throw new Error(`magmux mcp exited while waiting for ${what}\nstderr: ${this.stderr}`);
      await sleep(20);
    }
    throw new Error(`never saw ${what} within ${timeoutMs}ms (${this.messages.length} messages)\nstderr: ${this.stderr}`);
  }

  async reply(id: number, timeoutMs = 30_000): Promise<any> {
    const m = await this.await(`a response to id ${id}`, timeoutMs, (x) => x.id === id);
    if (m.error) throw new Error(`request ${id} failed: ${JSON.stringify(m.error)}`);
    return m.result ?? {};
  }

  async call(method: string, params?: unknown, timeoutMs = 30_000): Promise<any> {
    return this.reply(this.request(method, params), timeoutMs);
  }

  /** A notification for `method`, optionally narrowed by its params. */
  notification(method: string, timeoutMs = 30_000, narrow?: (p: any) => boolean, from = 0): Promise<any> {
    return this.await(
      narrow ? `${method} (narrowed)` : method,
      timeoutMs,
      (m) => m.method === method && (!narrow || narrow(m.params ?? {})),
      from,
    );
  }

  async handshake(clientName = "magmux-rc") {
    const init = await this.call("initialize", {
      protocolVersion: "2025-06-18",
      clientInfo: { name: clientName, version: "1.0" },
      capabilities: {},
    });
    this.send({ jsonrpc: "2.0", method: "notifications/initialized" });
    return init;
  }

  stop() {
    if (!this.proc || this.exited) return;
    try {
      this.proc.kill("SIGTERM");
    } catch {}
  }
}

export function toolNames(result: any): string[] {
  return (result?.tools ?? []).map((t: any) => t.name);
}

export function resourceURIs(result: any): string[] {
  return (result?.resources ?? []).map((r: any) => r.uri);
}

export function contentsText(result: any): string {
  return String(result?.contents?.[0]?.text ?? "");
}

export function toolText(result: any): string {
  return (result?.content ?? []).map((c: any) => String(c?.text ?? "")).join("\n");
}
