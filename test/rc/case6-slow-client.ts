#!/usr/bin/env bun
/**
 * RC case 6 — validation criterion 6: one WebSocket client that never reads,
 * beside a live one. The live one keeps receiving frames, and the socket keeps
 * answering.
 *
 * This is the failure mode the hub's whole design is about. Publishing is
 * non-blocking, every subscriber has its own FIFO, and a subscriber that cannot
 * keep up fills its queue and is CLOSED — alone. If any of that were wrong the
 * symptom would not be an error anywhere; it would be magmux going quiet for
 * everybody because one laptop lid shut.
 *
 * The stalled client is a raw TCP socket that performs the WebSocket handshake
 * by hand and is then PAUSED: no `data` handler, no reads, so the kernel buffer
 * fills and TCP back-pressure reaches magmux. A `new WebSocket` cannot be made
 * to do this — the runtime always drains it — which is why this one is written
 * against node:net.
 *
 * What is measured, while the stall is in place:
 *   - the live WS client's inter-frame gap,
 *   - the round-trip of a `list` on the UNIX SOCKET, the transport that has
 *     nothing to do with HTTP and must be unaffected,
 *   - the round-trip of a REST call.
 *
 * A number is asserted for each, because "it did not hang" is not a claim a
 * test can make by finishing.
 */
import crypto from "node:crypto";
import net from "node:net";
import {
  C, RemoteMagmux, buildMagmux, criterion, dumpStderr, firstSessionPane, frameText,
  header, report, sleep, type Check,
} from "./harness.ts";

// The pane prints hard and wide: numbered lines so the live client's progress
// is countable, padded so a frame is big enough that a reader which never reads
// fills its kernel buffer in seconds rather than minutes. The padding is built
// ONCE, outside the loop — a `$(...)` per line would make the shell, not
// magmux, the bottleneck and the case would measure nothing.
const CHATTY =
  `sh -c 'pad=$(printf "%0.sx" $(seq 1 240)); i=0; ` +
  `while :; do i=$((i+1)); echo "rc6 line $i $pad"; done'`;
const STALL_MS = 12_000;
const GAP_BUDGET_MS = 2_000;
const SOCKET_BUDGET_MS = 2_000;

header(
  "RC case 6 — a stalled WebSocket beside a live one",
  "validation criterion 6: the live client keeps receiving, and the socket keeps answering",
);

const bin = buildMagmux();
const m = new RemoteMagmux(bin, "slow");
const checks: Check[] = [];
let live: ReturnType<RemoteMagmux["ws"]> | null = null;
let stalled: StalledWS | null = null;

try {
  await m.start(["-e", CHATTY, "--label", "chatty"]);
  console.log(`\n${C.bold}  Setup${C.reset}`);
  console.log(`  ${C.grey}url  ${C.reset}${m.base}`);
  console.log(`  ${C.grey}pane ${C.reset}${CHATTY}`);

  // ── the stalled client ───────────────────────────────────────────────────
  stalled = new StalledWS(m.addr, m.token);
  const stalledPane = await stalled.connectAndWatch();
  console.log(`  ${C.grey}stalled client watching pane ${stalledPane} at 30fps, and reading NOTHING${C.reset}`);
  checks.push({
    label: "the stalled client is really watching before it stops reading",
    pass: stalledPane >= 0 && stalled.bytesRead > 0,
    detail: `pane ${stalledPane}, ${stalled.bytesRead} bytes read including a frame; paused from here on`,
  });

  // ── the live client ──────────────────────────────────────────────────────
  live = m.ws();
  await live.open();
  const agg = await live.seen((ev) => ev.type === "snapshot" && Array.isArray(ev.panes), 15_000, "the aggregate");
  const pane = firstSessionPane(agg.ev);
  await live.call("watch", { pane, fps: 30 });
  await live.seen((ev) => ev.type === "frame" && ev.pane === pane && ev.key === true, 20_000, "a keyframe");

  // ── measure, while the stall is in place ─────────────────────────────────
  console.log(`\n${C.bold}  Measuring for ${STALL_MS / 1000}s with the stalled client connected${C.reset}`);
  const t0 = Date.now();
  const frameAts: number[] = [];
  const restMs: number[] = [];
  const sockMs: number[] = [];
  const seen = new Set<string>();

  while (Date.now() - t0 < STALL_MS) {
    const before = live.messages.length;
    await sleep(250);
    for (const rec of live.messages.slice(before)) {
      if (rec.ev.type === "frame" && rec.ev.pane === pane) {
        frameAts.push(rec.at);
        for (const l of frameText(rec.ev).split("\n")) {
          const mm = /rc6 line (\d+)/.exec(l);
          if (mm) seen.add(mm[1]);
        }
      }
    }
    const r0 = Date.now();
    const r = await m.http("GET", "/v1/panes");
    if (r.status !== 200) throw new Error(`GET /v1/panes → ${r.status} while a client was stalled`);
    restMs.push(Date.now() - r0);

    const s0 = Date.now();
    const list = await socketList(m.sock);
    if (!Array.isArray(list?.result?.panes)) throw new Error(`the unix socket answered ${JSON.stringify(list)}`);
    sockMs.push(Date.now() - s0);
  }

  const gaps: number[] = [];
  for (let i = 1; i < frameAts.length; i++) gaps.push(frameAts[i] - frameAts[i - 1]);
  const maxGap = gaps.length ? Math.max(...gaps) : Infinity;
  const worstRest = Math.max(...restMs);
  const worstSock = Math.max(...sockMs);

  console.log(`  ${C.grey}live frames     ${C.reset}${frameAts.length} over ${((Date.now() - t0) / 1000).toFixed(1)}s, worst gap ${fmt(maxGap)}ms  ${C.grey}(budget ${GAP_BUDGET_MS}ms)${C.reset}`);
  console.log(`  ${C.grey}distinct lines  ${C.reset}${seen.size} of the pane's output reached the live client`);
  console.log(`  ${C.grey}unix socket     ${C.reset}${sockMs.length} \`list\` calls, worst ${worstSock}ms  ${C.grey}(budget ${SOCKET_BUDGET_MS}ms)${C.reset}`);
  console.log(`  ${C.grey}REST            ${C.reset}${restMs.length} calls, worst ${worstRest}ms  ${C.grey}(budget ${SOCKET_BUDGET_MS}ms)${C.reset}`);
  console.log(`  ${C.grey}stalled client  ${C.reset}read ${stalled.bytesRead} bytes; magmux ${stalled.closedByPeer ? "closed it" : "is still holding it"}`);

  checks.push({
    label: "the live client kept receiving frames throughout",
    pass: frameAts.length >= 10 && maxGap <= GAP_BUDGET_MS,
    detail: `${frameAts.length} frames, worst gap ${fmt(maxGap)}ms`,
  });
  checks.push({
    label: "the live client saw the pane's output, not a frozen screen",
    pass: seen.size >= 10,
    detail: `${seen.size} distinct output lines`,
  });
  checks.push({
    label: "the unix socket kept answering `list`",
    pass: sockMs.length > 0 && worstSock <= SOCKET_BUDGET_MS,
    detail: `${sockMs.length} calls, worst ${worstSock}ms, median ${median(sockMs)}ms`,
  });
  checks.push({
    label: "REST kept answering",
    pass: restMs.length > 0 && worstRest <= SOCKET_BUDGET_MS,
    detail: `${restMs.length} calls, worst ${worstRest}ms, median ${median(restMs)}ms`,
  });

  // The stalled subscriber is the one that pays, and whether magmux has already
  // shed it depends on how long the kernel is willing to buffer for it — on
  // loopback that can be megabytes, so it is REPORTED here rather than
  // required. The shedding rule itself (a full FIFO closes THAT Sub and no
  // other, with close code 1013) is driven directly in hub's own tests, where
  // the queue can be filled without going through a socket buffer at all.
  //
  // What this case asserts is the part only a real stack can show: that the
  // stall was contained.
  checks.push({
    label: "the stall was contained to the client that caused it",
    pass: !live.closed,
    detail: stalled.closedByPeer
      ? `magmux shed the stalled client (${stalled.bytesRead} bytes read); the live one is still open`
      : `magmux is still buffering for the stalled client (it read ${stalled.bytesRead} bytes); the live one is unaffected`,
  });

  // ── and the session survives it ──────────────────────────────────────────
  stalled.close();
  await sleep(500);
  const after = await m.http("GET", "/v1/panes");
  checks.push({
    label: "magmux is healthy after the stalled client goes away",
    pass: after.status === 200 && (after.json?.panes?.length ?? 0) > 0,
    detail: `${after.status}, ${after.json?.panes?.length} panes, live client closed=${live.closed}`,
  });

  const stillTyping = await m.http("POST", "/v1/ops/input", { body: { pane, text: "\r" } });
  checks.push({
    label: "the pane still takes input",
    pass: stillTyping.status === 200,
    detail: `${stillTyping.status} ${JSON.stringify(stillTyping.json?.result)}`,
  });
} catch (err) {
  checks.push({ label: "the case ran to completion", pass: false, detail: String(err) });
  dumpStderr(m);
} finally {
  stalled?.close();
  live?.close();
  await m.stop();
  m.cleanup();
}

const code = report(checks);
criterion(
  6,
  "Slow client: a stalled WS does not stop the live one or the socket",
  code === 0,
  `${checks.filter((c) => c.pass).length}/${checks.length} checks`,
);
process.exit(code);

// ── helpers ─────────────────────────────────────────────────────────────────

function fmt(n: number): string {
  return Number.isFinite(n) ? String(Math.round(n)) : "∞";
}

function median(xs: number[]): number {
  const s = [...xs].sort((a, b) => a - b);
  return s.length ? s[Math.floor(s.length / 2)] : -1;
}

/** One `list` over the unix socket, the way any pre-HTTP client does it. */
function socketList(sock: string, timeoutMs = 10_000): Promise<any> {
  return new Promise((resolve, reject) => {
    const c = net.createConnection(sock);
    let buf = "";
    const timer = setTimeout(() => {
      c.destroy();
      reject(new Error(`the unix socket did not answer \`list\` within ${timeoutMs}ms`));
    }, timeoutMs);
    c.on("connect", () => c.write(JSON.stringify({ type: "list", id: "rc6" }) + "\n"));
    c.on("data", (b) => {
      buf += b.toString();
      let nl: number;
      while ((nl = buf.indexOf("\n")) >= 0) {
        const line = buf.slice(0, nl);
        buf = buf.slice(nl + 1);
        let ev: any;
        try {
          ev = JSON.parse(line);
        } catch {
          continue;
        }
        if (ev.type === "reply" && ev.id === "rc6") {
          clearTimeout(timer);
          c.destroy();
          return resolve(ev);
        }
      }
    });
    c.on("error", (e) => {
      clearTimeout(timer);
      reject(e);
    });
  });
}

/**
 * A WebSocket that handshakes, subscribes, and then never reads another byte.
 *
 * node:net rather than the platform WebSocket, because the platform one always
 * drains its socket: there is no way to ask it to stop, and a client that keeps
 * reading is not the client this case is about. Here the socket is `pause()`d
 * immediately after the handshake, so the receive buffer fills, TCP advertises
 * a zero window, and magmux's write blocks exactly as it would against a
 * wedged browser.
 */
class StalledWS {
  private sock: net.Socket | null = null;
  bytesRead = 0;
  closedByPeer = false;

  constructor(
    readonly addr: string,
    readonly token: string,
  ) {}

  async connectAndWatch(): Promise<number> {
    const [host, port] = this.addr.split(":");
    const key = crypto.randomBytes(16).toString("base64");
    const sock = net.createConnection({ host, port: Number(port) });
    this.sock = sock;
    sock.on("close", () => (this.closedByPeer = true));
    sock.on("error", () => (this.closedByPeer = true));
    await new Promise<void>((res, rej) => {
      sock.once("connect", () => res());
      sock.once("error", rej);
    });

    sock.write(
      [
        "GET /v1/ws HTTP/1.1",
        `Host: ${this.addr}`,
        "Upgrade: websocket",
        "Connection: Upgrade",
        `Sec-WebSocket-Key: ${key}`,
        "Sec-WebSocket-Version: 13",
        `Sec-WebSocket-Protocol: magmux.v1, magmux.auth.${this.token}`,
        "",
        "",
      ].join("\r\n"),
    );

    // Read the 101, send the watch, and then read just far enough to see a
    // frame come back before going deaf.
    //
    // That last part is what stops this being a case that tests nothing: a
    // handshake that failed, or a `watch` that was refused, would leave a
    // client which is quiet because there is nothing for it rather than
    // because it is not listening. Server-to-client frames are unmasked, so
    // the JSON is plainly there in the bytes and no decoder is needed to spot
    // it.
    const pane = 0;
    let sentWatch = false;
    await new Promise<void>((res, rej) => {
      let buf = "";
      const onData = (b: Buffer) => {
        this.bytesRead += b.length;
        buf += b.toString("latin1");
        if (!sentWatch && buf.includes("\r\n\r\n")) {
          if (!/^HTTP\/1\.1 101/.test(buf)) {
            sock.off("data", onData);
            return rej(new Error(`the stalled client's handshake failed: ${buf.split("\r\n")[0]}`));
          }
          sentWatch = true;
          this.writeText(JSON.stringify({ id: 1, op: "watch", args: { pane, fps: 30 } }));
        }
        if (sentWatch && buf.includes(`"type":"frame"`)) {
          sock.off("data", onData);
          res();
        }
      };
      sock.on("data", onData);
      sock.once("error", rej);
      setTimeout(() => rej(new Error("the stalled client saw no frame within 20s; it is not really watching")), 20_000);
    });

    // And now it stops reading, for good. The kernel buffer fills from here.
    sock.pause();
    return pane;
  }

  /** A masked client text frame — RFC 6455 requires the mask from a client. */
  private writeText(s: string) {
    const payload = Buffer.from(s, "utf8");
    const mask = crypto.randomBytes(4);
    const head: number[] = [0x81];
    if (payload.length < 126) head.push(0x80 | payload.length);
    else if (payload.length < 65536) head.push(0x80 | 126, payload.length >> 8, payload.length & 0xff);
    else throw new Error("the stalled client never sends anything that large");
    const masked = Buffer.from(payload);
    for (let i = 0; i < masked.length; i++) masked[i] ^= mask[i % 4];
    this.sock!.write(Buffer.concat([Buffer.from(head), mask, masked]));
  }

  close() {
    try {
      this.sock?.destroy();
    } catch {}
  }
}
