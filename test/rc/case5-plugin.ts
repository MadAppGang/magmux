#!/usr/bin/env bun
/**
 * RC case 5 — validation criterion 5: the demo plugin's op is callable over
 * every transport, and everything it says comes back on every transport.
 *
 *   HTTP   POST /v1/ops/ticket.run_ticket     events on GET /v1/events (SSE)
 *   WS     {"op":"ticket.run_ticket"}         events on the same socket
 *   MCP    tools/call ticket__run_ticket      events on magmux://…/plugin/ticket/events
 *   RTDB   a signed command, op ticket.run_ticket    events in the mirror's ring
 *
 * The plugin is a bun process magmux started, with no model, no network and no
 * API key: `examples/plugins/ticket-runner`. It opens a pane with
 * `controller:"self"`, waits for its agent's prompt, sends the ticket, and then
 * pushes an `awaiting_input` snapshot carrying the agent's `DONE:` line.
 *
 * That last part is the claim worth the whole case. `awaiting_input` is what a
 * driver waits for, and here it is a statement about a session magmux is NOT
 * itself following, made by a separate process, reconciled with what the
 * terminal actually saw — and it has to reach a browser, an agent and a
 * database identically, because they are four views of one hub and not four
 * implementations of one idea.
 *
 * The RTDB quarter is gated on MAGMUX_FIREBASE_EMULATOR=1 (firebase-tools and a
 * JDK 21+). Without it the case proves three transports and says so.
 */
import fs from "node:fs";
import path from "node:path";
import {
  C, RemoteMagmux, TICKET_PLUGIN, buildMagmux, criterion, dumpStderr, header,
  hostname, report, until, writeTokenFile, type Check,
} from "./harness.ts";
import { MCPChild, contentsText, resourceURIs, toolNames, toolText } from "./mcpclient.ts";
import { Emulator, EmuREST, admin, emulatorEnabled, idToken, sign } from "./emulator.ts";

const OWNER = "rc-owner-5";
const HOST = `rc5-${hostname()}`;
const ROOT = "magmux";
const KEY = "magmux-rc5-emulator-key-not-a-real-secret";
const SESSION = `rc5-${process.pid}`;
const withRTDB = emulatorEnabled();

header(
  "RC case 5 — the demo plugin over HTTP, WebSocket, MCP and Firebase",
  `validation criterion 5${withRTDB ? "" : " (RTDB skipped: set MAGMUX_FIREBASE_EMULATOR=1)"}`,
);

const bin = buildMagmux();
const emu = new Emulator();
const m = new RemoteMagmux(bin, "plugin");
const checks: Check[] = [];
let ws: ReturnType<RemoteMagmux["ws"]> | null = null;
let mcp: MCPChild | null = null;
let sse: SSEStream | null = null;
const transports: string[] = [];

try {
  if (!fs.existsSync(TICKET_PLUGIN)) throw new Error(`the demo plugin is missing: ${TICKET_PLUGIN}`);

  const args = ["-e", "cat", "--label", "host", "--id", SESSION, "--plugin", `bun ${TICKET_PLUGIN}`];
  let sess = "";
  let db: EmuREST | null = null;

  if (withRTDB) {
    console.log(`\n${C.bold}  Emulator${C.reset}`);
    const t0 = Date.now();
    await emu.start();
    console.log(`  ${C.green}✓${C.reset} answering after ${Date.now() - t0}ms`);
    const keyFile = path.join(m.dir, "cmd.key");
    writeTokenFile(keyFile, KEY);
    const cfgPath = path.join(m.dir, "firebase.json");
    fs.writeFileSync(
      cfgPath,
      JSON.stringify({
        root: ROOT,
        host: HOST,
        emulator: { host: "127.0.0.1:9000", ns: "demo-magmux-default-rtdb" },
        frameFps: 2,
        commands: { enabled: true, owners: [OWNER], keyFile, allowOps: ["list", "ticket.*"] },
      }),
    );
    args.push("--firebase", cfgPath);
  }

  await m.start(args);
  console.log(`\n${C.bold}  Setup${C.reset}`);
  console.log(`  ${C.grey}url    ${C.reset}${m.base}`);
  console.log(`  ${C.grey}plugin ${C.reset}bun ${path.relative(process.cwd(), TICKET_PLUGIN)}`);
  if (withRTDB) {
    const mirror = await m.waitForLine(/mirroring to (\S+)/, 20_000);
    sess = new URL(mirror[1]).pathname.replace(/^\/+|\.json$/g, "");
    db = admin();
    console.log(`  ${C.grey}mirror ${C.reset}${sess}`);
  }

  // The plugin is a separate process magmux has just started, so its ops appear
  // when it registers, not when magmux binds. Poll for it.
  await until("ticket.run_ticket to be registered", 60_000, async () => {
    const r = await m.http("GET", "/v1/ops");
    return (r.json?.ops ?? []).some((o: any) => o.name === "ticket.run_ticket");
  });
  const ops = (await m.http("GET", "/v1/ops")).json.ops;
  checks.push({
    label: "the plugin's ops join the catalogue every transport reads",
    pass: ["ticket.run_ticket", "ticket.status", "ticket.cancel"].every((n) => ops.some((o: any) => o.name === n)),
    detail: ops.filter((o: any) => o.source === "ticket").map((o: any) => o.name).join(" "),
  });

  // ── HTTP, with its events on SSE ─────────────────────────────────────────
  console.log(`\n${C.bold}  HTTP${C.reset}`);
  sse = new SSEStream(`${m.base}/v1/events`, m.token);
  await sse.open();

  const httpCall = await m.http("POST", "/v1/ops/ticket.run_ticket", { body: { title: "over http" } });
  const httpPane = httpCall.json?.result?.pane;
  checks.push({
    label: "HTTP: POST /v1/ops/ticket.run_ticket runs the plugin's op",
    pass: httpCall.status === 200 && httpCall.json?.ok === true && typeof httpPane === "number",
    detail: `${httpCall.status} ${JSON.stringify(httpCall.json?.result)}`,
  });
  transports.push("http");

  const httpProgress = await sse.await(
    (ev) => ev.type === "plugin" && ev.plugin === "ticket" && ev.event === "progress" && ev.data?.title === "over http",
    60_000,
    "a `progress` event for the HTTP ticket on SSE",
  );
  const httpDone = await sse.await(
    (ev) => ev.type === "snapshot" && ev.pane === httpPane && ev.state === "awaiting_input",
    120_000,
    "the pane settling at awaiting_input on SSE",
  );
  checks.push({
    label: "HTTP/SSE: the plugin's `progress` events arrive",
    pass: !!httpProgress,
    detail: `step=${httpProgress.data?.step} ${JSON.stringify(httpProgress.data)}`,
  });
  checks.push({
    label: "HTTP/SSE: the awaiting_input snapshot carries the agent's DONE: line",
    pass: String(httpDone.response ?? "").startsWith("DONE: over http") && httpDone.controller === "plugin:ticket",
    detail: `controller=${httpDone.controller} response="${httpDone.response}"`,
  });

  // ── WebSocket ────────────────────────────────────────────────────────────
  console.log(`\n${C.bold}  WebSocket${C.reset}`);
  ws = m.ws();
  await ws.open();
  await ws.call("hello", { client: "magmux-rc/case5" });

  const wsMark = ws.messages.length;
  const wsRes = await ws.call("ticket.run_ticket", { title: "over ws" }, 120_000);
  const wsPane = wsRes.pane;
  checks.push({
    label: "WS: the plugin's op is callable by name",
    pass: typeof wsPane === "number",
    detail: JSON.stringify(wsRes),
  });
  transports.push("ws");

  const wsProgress = await ws.seen(
    (ev) => ev.type === "plugin" && ev.plugin === "ticket" && ev.event === "progress" && ev.data?.title === "over ws",
    60_000,
    "a `progress` event for the WS ticket",
  );
  const wsDone = await ws.seen(
    (ev) => ev.type === "snapshot" && ev.pane === wsPane && ev.state === "awaiting_input",
    120_000,
    "the WS ticket's pane settling",
  );
  checks.push({
    label: "WS: the plugin's `progress` events arrive on the same connection",
    pass: ws.messages.indexOf(wsProgress) >= wsMark,
    detail: `step=${wsProgress.ev.data?.step} ${JSON.stringify(wsProgress.ev.data)}`,
  });
  checks.push({
    label: "WS: the awaiting_input snapshot carries the agent's DONE: line",
    pass: String(wsDone.ev.response ?? "").startsWith("DONE: over ws") && wsDone.ev.controller === "plugin:ticket",
    detail: `controller=${wsDone.ev.controller} response="${wsDone.ev.response}"`,
  });

  // ── MCP ──────────────────────────────────────────────────────────────────
  console.log(`\n${C.bold}  MCP${C.reset}`);
  mcp = new MCPChild(bin, m.sockDir);
  mcp.start();
  await mcp.handshake("magmux-rc/case5");

  let tools: string[] = [];
  await until("ticket__run_ticket to appear as an MCP tool", 60_000, async () => {
    tools = toolNames(await mcp!.call("tools/list"));
    return tools.includes("ticket__run_ticket");
  });
  checks.push({
    label: "MCP: the plugin op is a tool, spelled ticket__run_ticket",
    pass: tools.includes("ticket__run_ticket"),
    detail: tools.filter((t) => t.startsWith("ticket__")).join(" ") || tools.join(" "),
  });

  const eventsURI = `magmux://${SESSION}/plugin/ticket/events`;
  const uris = resourceURIs(await mcp.call("resources/list"));
  checks.push({
    label: "MCP: the plugin's events are a resource",
    pass: uris.includes(eventsURI),
    detail: uris.filter((u) => u.includes("/plugin/")).join(" ") || "(none)",
  });
  await mcp.call("resources/subscribe", { uri: eventsURI });

  const mcpMark = mcp.mark;
  const mcpRes = await mcp.call("tools/call", {
    name: "ticket__run_ticket",
    arguments: { title: "over mcp" },
  }, 180_000);
  checks.push({
    label: "MCP: tools/call ticket__run_ticket runs it",
    pass: mcpRes.isError !== true,
    detail: toolText(mcpRes).slice(0, 80),
  });
  transports.push("mcp");

  await mcp.notification("notifications/resources/updated", 120_000, (p) => p.uri === eventsURI, mcpMark);
  const ringBody = contentsText(await mcp.call("resources/read", { uri: eventsURI }));
  const ring = JSON.parse(ringBody);
  const mcpProgress = (ring.events ?? []).filter((e: any) => e.event === "progress");
  checks.push({
    label: "MCP: the plugin's `progress` events reach the client after the tool result",
    pass: mcpProgress.length > 0,
    detail: `${ring.events?.length} events in the ring, ${mcpProgress.length} progress, epoch ${ring.epoch}`,
  });

  const mcpPane = JSON.parse(toolText(mcpRes))?.pane ?? mcpProgress.at(-1)?.pane;
  const mcpDone = await ws.seen(
    (ev) => ev.type === "snapshot" && ev.pane === mcpPane && ev.state === "awaiting_input" && String(ev.response ?? "").startsWith("DONE: over mcp"),
    180_000,
    "the MCP ticket's pane settling",
  );
  const paneURI = `magmux://${SESSION}/pane/${mcpPane}/screen`;
  const screen = contentsText(await mcp.call("resources/read", { uri: paneURI }));
  checks.push({
    label: "MCP: the DONE: line is on the pane resource, and the snapshot agrees",
    pass: screen.includes("DONE: over mcp") && String(mcpDone.ev.response ?? "").startsWith("DONE: over mcp"),
    detail: `resource has the line; snapshot response="${mcpDone.ev.response}" controller=${mcpDone.ev.controller}`,
  });

  // ── Firebase ─────────────────────────────────────────────────────────────
  if (withRTDB && db) {
    console.log(`\n${C.bold}  Firebase${C.reset}`);
    const owner = new EmuREST({ idToken: idToken(OWNER) });
    const ts = Date.now();
    const nonce = "rc5-nonce-00000000001";
    const fbArgs = JSON.stringify({ title: "over rtdb" });
    const putCode = await owner.put(`${sess}/commands/-RC5run0001`, {
      uid: OWNER,
      ts,
      nonce,
      op: "ticket.run_ticket",
      args: fbArgs,
      sig: sign(KEY, HOST, path.basename(sess), OWNER, ts, nonce, "ticket.run_ticket", fbArgs),
    });
    checks.push({
      label: "RTDB: a signed command may name a plugin op",
      pass: putCode === 200,
      detail: `HTTP ${putCode}, allowOps had "ticket.*"`,
    });

    let fbResult: any = null;
    await until("the command's result", 180_000, async () => {
      fbResult = await db!.get(`${sess}/results/-RC5run0001`);
      return fbResult?.state === "done";
    });
    const fbPane = JSON.parse(String(fbResult?.result ?? "{}"))?.pane;
    checks.push({
      label: "RTDB: the plugin op ran and its result mirrored",
      pass: fbResult?.ok === true && typeof fbPane === "number",
      detail: `ok=${fbResult?.ok} result=${String(fbResult?.result ?? "").slice(0, 60)}`,
    });
    transports.push("firebase");

    // The op key in the catalogue uses MCP's spelling, because `.` is one of
    // the six characters an RTDB key may not contain.
    const fbOps = await db.get(`${sess}/ops`);
    checks.push({
      label: "RTDB: the plugin's op is in the mirrored catalogue as ticket__run_ticket",
      pass: typeof fbOps?.ticket__run_ticket === "string",
      detail: Object.keys(fbOps ?? {}).filter((k) => k.startsWith("ticket__")).join(" "),
    });

    let fbEvents: any = null;
    await until("the plugin's progress events in the mirror's ring", 120_000, async () => {
      fbEvents = await db!.get(`${sess}/events`);
      return Object.values(fbEvents ?? {}).some(
        (e: any) => e?.type === "plugin" && e?.plugin === "ticket" && e?.event === "progress",
      );
    });
    const progressRecs = Object.values(fbEvents ?? {}).filter(
      (e: any) => e?.type === "plugin" && e?.event === "progress",
    );
    checks.push({
      label: "RTDB: the plugin's `progress` events are in the event ring",
      pass: progressRecs.length > 0,
      detail: `${Object.keys(fbEvents ?? {}).length} events, ${progressRecs.length} progress; plugin and event are their own fields`,
    });

    let fbState: any = null;
    await until("the pane settling at awaiting_input in the mirror", 180_000, async () => {
      fbState = await db!.get(`${sess}/panes/p${fbPane}/state`);
      return fbState?.state === "awaiting_input" && String(fbState?.response ?? "").startsWith("DONE: over rtdb");
    });
    checks.push({
      label: "RTDB: the awaiting_input state carries the agent's DONE: line",
      pass: fbState?.state === "awaiting_input" && String(fbState?.response ?? "").startsWith("DONE: over rtdb"),
      detail: `state=${fbState?.state} controller=${fbState?.controller} response="${fbState?.response}"`,
    });
  } else {
    console.log(`\n  ${C.yellow}SKIP${C.reset} ${C.grey}Firebase — set MAGMUX_FIREBASE_EMULATOR=1 (needs firebase-tools and a JDK 21+)${C.reset}`);
  }

  // ── the plugin's own view has to agree with magmux's ──────────────────────
  const status = await m.http("POST", "/v1/ops/ticket.status", { body: {} });
  const tickets = status.json?.result?.tickets ?? [];
  checks.push({
    label: "the plugin's own `status` agrees with what magmux reported",
    pass:
      tickets.length === transports.length &&
      tickets.every((t: any) => t.state === "done" && String(t.response).startsWith("DONE:")),
    detail: tickets.map((t: any) => `${t.ticket}:${t.state}@${t.pane}`).join(" ") || JSON.stringify(status.json),
  });
} catch (err) {
  checks.push({ label: "the case ran to completion", pass: false, detail: String(err) });
  dumpStderr(m);
} finally {
  sse?.close();
  ws?.close();
  mcp?.stop();
  await m.stop();
  m.cleanup();
  emu.stop();
}

const code = report(checks);
criterion(
  5,
  "Demo plugin: callable and observable over " + (withRTDB ? "all four transports" : "HTTP, WS and MCP"),
  code === 0,
  `${checks.filter((c) => c.pass).length}/${checks.length} checks; transports proved: ${transports.join(", ") || "none"}` +
    (withRTDB ? "" : " (Firebase skipped — MAGMUX_FIREBASE_EMULATOR is not 1)"),
);
process.exit(code);

// ── a minimal EventSource ───────────────────────────────────────────────────

/**
 * GET /v1/events read as a stream.
 *
 * Written by hand rather than with the platform EventSource because a bearer
 * token cannot be put on one — which is exactly why magmux has tickets — and
 * because the case wants the `event:` name as well as the data.
 */
class SSEStream {
  readonly events: { at: number; name: string; ev: any }[] = [];
  private ctl = new AbortController();
  private waiters: { pred: (ev: any, name: string) => boolean; resolve: (v: any) => void; timer: any }[] = [];

  constructor(
    readonly url: string,
    readonly token: string,
  ) {}

  async open(timeoutMs = 20_000) {
    const res = await fetch(this.url, {
      headers: { Authorization: `Bearer ${this.token}`, Accept: "text/event-stream" },
      signal: this.ctl.signal,
    });
    if (!res.ok || !res.body) throw new Error(`GET ${this.url} → ${res.status}`);
    void this.pump(res.body);
    // The aggregate is the first event on every transport, so its arrival is
    // the stream being live.
    await this.await((_, name) => name === "snapshot", timeoutMs, "the connect-time aggregate");
  }

  private async pump(body: ReadableStream<Uint8Array>) {
    const dec = new TextDecoder();
    let buf = "";
    try {
      for await (const chunk of body as any) {
        buf += dec.decode(chunk, { stream: true });
        let i: number;
        while ((i = buf.indexOf("\n\n")) >= 0) {
          this.record(buf.slice(0, i));
          buf = buf.slice(i + 2);
        }
      }
    } catch {
      // The stream ended, which for a case that is finishing is not an error.
    }
  }

  private record(block: string) {
    let name = "message";
    let data = "";
    for (const line of block.split("\n")) {
      if (line.startsWith("event:")) name = line.slice(6).trim();
      else if (line.startsWith("data:")) data += line.slice(5).trim();
    }
    if (!data) return;
    let ev: any;
    try {
      ev = JSON.parse(data);
    } catch {
      return;
    }
    const rec = { at: Date.now(), name, ev };
    this.events.push(rec);
    for (let i = this.waiters.length - 1; i >= 0; i--) {
      const w = this.waiters[i];
      if (w.pred(ev, name)) {
        this.waiters.splice(i, 1);
        clearTimeout(w.timer);
        w.resolve(ev);
      }
    }
  }

  await(pred: (ev: any, name: string) => boolean, timeoutMs: number, what: string): Promise<any> {
    for (const e of this.events) if (pred(e.ev, e.name)) return Promise.resolve(e.ev);
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => {
        const i = this.waiters.findIndex((w) => w.timer === timer);
        if (i >= 0) this.waiters.splice(i, 1);
        reject(new Error(`never saw ${what} on SSE within ${timeoutMs}ms (${this.events.length} events)`));
      }, timeoutMs);
      this.waiters.push({ pred, resolve, timer });
    });
  }

  close() {
    try {
      this.ctl.abort();
    } catch {}
  }
}
