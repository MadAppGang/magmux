#!/usr/bin/env bun
/**
 * RC case 2 — validation criterion 2: watch a shell pane over a WebSocket,
 * type into it, and see the result come back as a frame within 1 second.
 *
 * The one-second budget is the whole claim. magmux's frame path is a hook on
 * the pane's readLoop, a framer that diffs against the last frame sent, and a
 * per-(Sub, pane) lane; if any of those three stopped being live the case still
 * passes a "did a frame ever arrive" test, and only a MEASURED latency catches
 * it. So the number is printed, and it is asserted.
 *
 * The measurement starts when the `input` request is written to the socket and
 * stops at the first frame whose text contains the marker on a line of its
 * OWN — the terminal echoes the command line too, and the echo is not evidence
 * the shell ran anything.
 *
 * The credential goes in Sec-WebSocket-Protocol as `magmux.auth.<token>`,
 * because a browser cannot set an Authorization header on a WebSocket and the
 * browser path is the one worth testing.
 */
import {
  C, RemoteMagmux, buildMagmux, criterion, dumpStderr, frameText, firstSessionPane,
  header, report, type Check,
} from "./harness.ts";

const MARKER = "MAGMUX_RC_OK";
const BUDGET_MS = 1000;

header(
  "RC case 2 — WebSocket: watch, type, and measure the frame latency",
  `validation criterion 2, budget ${BUDGET_MS}ms for ${MARKER} to come back as a frame`,
);

const bin = buildMagmux();
const m = new RemoteMagmux(bin, "ws");
const checks: Check[] = [];
let ws: ReturnType<RemoteMagmux["ws"]> | null = null;

try {
  await m.start(["-e", "sh", "--label", "shell"]);
  console.log(`\n${C.bold}  Setup${C.reset}`);
  console.log(`  ${C.grey}url   ${C.reset}${m.base.replace(/^http/, "ws")}/v1/ws`);
  console.log(`  ${C.grey}auth  ${C.reset}Sec-WebSocket-Protocol: magmux.v1, magmux.auth.<token>`);

  ws = m.ws();
  await ws.open();

  // The aggregate is the first message on every transport — a client seeds its
  // whole pane map from it and is never told that state again except on change.
  const agg = await ws.seen((ev) => ev.type === "snapshot" && Array.isArray(ev.panes), 15_000, "the connect-time aggregate");
  checks.push({
    label: "the aggregate is the first message",
    pass: ws.messages[0]?.ev?.type === "snapshot" && Array.isArray(ws.messages[0]?.ev?.panes),
    detail: `first message type=${ws.messages[0]?.ev?.type}, ${agg.ev.panes.length} panes`,
  });

  const pane = firstSessionPane(agg.ev);

  const hello = await ws.call("hello", { client: "magmux-rc/case2" });
  checks.push({ label: "`hello` names the client for the panel", pass: true, detail: JSON.stringify(hello) });

  // watch, then wait for the keyframe. A client sizes itself from the reply and
  // clears its screen for the keyframe; only then is it ready to type.
  const info = await ws.call("watch", { pane, fps: 30 });
  checks.push({
    label: "the watch reply carries the pane's geometry",
    pass: info.rows > 0 && info.cols > 0 && info.pane === pane,
    detail: `pane=${info.pane} ${info.rows}x${info.cols} mode=${info.mode} fps=${info.fps}`,
  });

  const key = await ws.seen((ev) => ev.type === "frame" && ev.pane === pane && ev.key === true, 20_000, "a keyframe");
  checks.push({
    label: "the first frame is a keyframe covering the whole pane",
    pass: key.ev.key === true && key.ev.lines.length === key.ev.rows,
    detail: `seq=${key.ev.seq} key=${key.ev.key} lines=${key.ev.lines.length}/${key.ev.rows}`,
  });

  // The barrier, measured only once a frame has actually arrived — asked any
  // earlier there is no frame to be out of order with, and the check passes
  // vacuously against a magmux that has no barrier at all.
  const firstFrameIdx = ws.messages.findIndex((x) => x.ev.type === "frame");
  const watchReplyIdx = ws.messages.findIndex((x) => x.ev.type === "reply" && x.ev.result?.pane === pane && x.ev.result?.rows);
  checks.push({
    label: "no frame precedes the watch reply that created it",
    pass: firstFrameIdx > 0 && watchReplyIdx >= 0 && watchReplyIdx < firstFrameIdx,
    detail: `watch reply at message #${watchReplyIdx}, first frame at #${firstFrameIdx}`,
  });

  // Let the shell settle on its first prompt, so the frame measured below is
  // the one this case's own input produced and not the prompt arriving late.
  // This is a POLL on the pane going quiet, not a fixed sleep.
  await quiet(ws, pane, 400, 15_000);

  // ── the measurement ──────────────────────────────────────────────────────
  const started = Date.now();
  const inputID = ws.send("input", { pane, text: `echo ${MARKER}\r` });

  const echoed = await ws.await(
    (ev) => ev.type === "frame" && ev.pane === pane && frameText(ev).split("\n").some((l) => l.trim() === MARKER),
    20_000,
    `a frame carrying ${MARKER} on a line of its own`,
  );
  const frameMs = echoed.at - started;

  const reply = ws.messages.find((x) => x.ev.type === "reply" && String(x.ev.id) === String(inputID));
  const replyMs = reply ? reply.at - started : -1;

  console.log(`\n${C.bold}  Measured${C.reset}`);
  console.log(`  ${C.grey}input reply      ${C.reset}${replyMs}ms  ${C.grey}(${reply?.ev?.result?.bytes} bytes reached the PTY)${C.reset}`);
  console.log(`  ${C.grey}frame with echo  ${C.reset}${frameMs}ms  ${C.grey}(budget ${BUDGET_MS}ms)${C.reset}`);
  console.log(`  ${C.grey}frames received  ${C.reset}${ws.messages.filter((x) => x.ev.type === "frame").length}`);

  checks.push({
    label: "`input` is acknowledged with the byte count it wrote",
    pass: reply?.ev?.ok === true && reply.ev.result?.bytes === `echo ${MARKER}\r`.length,
    detail: `ok=${reply?.ev?.ok} bytes=${reply?.ev?.result?.bytes} want ${`echo ${MARKER}\r`.length}`,
  });
  checks.push({
    label: `the shell's output came back as a frame within ${BUDGET_MS}ms`,
    pass: frameMs <= BUDGET_MS,
    detail: `${frameMs}ms`,
  });
  checks.push({
    label: "the frame is a diff, not a fresh keyframe every tick",
    pass: echoed.ev.key === false,
    detail: `key=${echoed.ev.key} lines=${echoed.ev.lines.length} of ${echoed.ev.rows} rows, seq=${echoed.ev.seq}`,
  });

  // `unwatch` means it: no more frames for that pane.
  await ws.call("unwatch", { pane });
  const before = ws.messages.filter((x) => x.ev.type === "frame").length;
  await ws.call("input", { pane, text: `echo MAGMUX_AFTER_UNWATCH\r` });
  await new Promise((r) => setTimeout(r, 750));
  const after = ws.messages.filter((x) => x.ev.type === "frame").length;
  checks.push({
    label: "`unwatch` stops the frames",
    pass: after === before,
    detail: `${before} frames before, ${after} after typing with the watch off`,
  });
} catch (err) {
  checks.push({ label: "the case ran to completion", pass: false, detail: String(err) });
  dumpStderr(m);
} finally {
  ws?.close();
  await m.stop();
  m.cleanup();
}

const code = report(checks);
criterion(
  2,
  `WebSocket: watch, type ${MARKER}, frame back inside ${BUDGET_MS}ms`,
  code === 0,
  `${checks.filter((c) => c.pass).length}/${checks.length} checks`,
);
process.exit(code);

/** Wait until the pane has produced no frame for `idleMs`. A poll, not a sleep. */
async function quiet(ws: ReturnType<RemoteMagmux["ws"]>, pane: number, idleMs: number, within: number) {
  const deadline = Date.now() + within;
  while (Date.now() < deadline) {
    const last = [...ws.messages].reverse().find((x) => x.ev.type === "frame" && x.ev.pane === pane);
    if (last && Date.now() - last.at >= idleMs) return;
    await new Promise((r) => setTimeout(r, 25));
  }
}
