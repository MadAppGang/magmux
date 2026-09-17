#!/usr/bin/env bun
/**
 * Automated checks for the remote-control demo.
 *
 * The demo is a SECOND implementation of magmux's frame protocol, living
 * outside the Go tests that prove the first one. That is the risk this file
 * exists for: a decoder that is subtly wrong about a colour or a column does
 * not fail, it just draws the wrong picture, and the person who notices is a
 * human at a screenshot. So the pure decoder is exercised against hand-built
 * frames, and then against a REAL magmux binary over the REAL WebSocket, with
 * the same read-only view token the demo hands its two clients.
 *
 * It deliberately does NOT live in `test/rc/`. That directory is the six
 * remote-control validation criteria and `run-all.ts` parses `__RC__ <n>` out of
 * them; a seventh entry would change what that suite claims to be.
 *
 * All eight checks of architecture.md's "Verification" section live here.
 *
 * Two of them are only meaningful against a running system, and both are the
 * demo's central claims rather than details of it:
 *
 *   check 4  ONE pane, TWO credentials: a full-token connection types, and the
 *            READ-ONLY one sees the result. That is the whole hexagon — one
 *            hub, one framer, two subscribers — measured rather than asserted,
 *            with the latency printed as `test/rc/case2-ws.ts` prints it.
 *   check 6  magmux is killed and the client SAYS SO. Silence is never the
 *            signal here — an idle pane legitimately produces no frames — so
 *            the one failure this demo must not have is a still picture of a
 *            magmux that died, and criterion 8 is that failure, automated.
 *
 * Check 9 is the ninth, and it is about the LAUNCHER rather than about the
 * protocol: `demo/rc/demo.sh` run for real, in its NO_BROWSER=1 NO_ATTACH=1
 * shape, under a per-run id. Everything above proves the parts; that one
 * proves the one file nothing else executes actually assembles them, that the
 * detached session survives being unattached, that the colour card reaches
 * the magmux pane AND its mirror, that the DRIVER came up holding the full
 * session token, that the ticket-runner plugin registered so the driver's
 * item 10 has something real to call, and that the teardown leaves nothing
 * behind. It SKIPS, with a reason, when tmux is not installed.
 */
import { execFileSync, spawn, spawnSync, type ChildProcess } from "node:child_process";
import crypto from "node:crypto";
import fs from "node:fs";
import path from "node:path";

import {
  C,
  RemoteMagmux,
  WSClient,
  buildMagmux,
  dumpStderr,
  frameText,
  header,
  report,
  sleep,
  until,
  type Check,
} from "../../test/rc/harness.ts";
import {
  ATTR_BOLD,
  ATTR_DIM,
  ATTR_ITALIC,
  ATTR_REVERSE,
  ATTR_UNDERLINE,
  ATTR_BLINK,
  ATTR_INVIS,
  ATTR_STRIKE,
  ATTR_OVERLINE,
  applyFrame,
  cellCols,
  classifyColour,
  newScreen,
  rowSpans,
  screenText,
  type FrameMessage,
} from "./frame.ts";

const HERE = import.meta.dir;
const checks: Check[] = [];

header(
  "demo/rc selftest — the shared decoder, the real wire, and the launcher",
  "architecture.md's eight checks, plus demo.sh run for real, against a real binary",
);

function check(label: string, pass: boolean, detail: string) {
  checks.push({ label, pass, detail });
}

/** Strip SGR escapes. Declared here because checks above and below both use it. */
const noAnsi = (s: string) => s.replace(/\x1b\[[0-9;]*m/g, "");

/**
 * The driver, built once and reused.
 *
 * `demo/rc/driver` is a Bubble Tea TUI and a Go MODULE OF ITS OWN — see the
 * go.mod check at the end of this file for why — so it has to be compiled
 * before anything can run it. Built rather than `go run` for the same reason
 * `demo.sh` builds it: `go run` links a throwaway binary on every invocation,
 * and this file invokes it several times.
 *
 * The output goes where demo.sh puts it, so a selftest run and a demo run share
 * one binary and neither can be testing a different build from the other.
 */
let driverBinPath: string | null = null;
function driverBin(): string {
  if (driverBinPath) return driverBinPath;
  const root = path.resolve(HERE, "../..");
  const out = path.join(root, ".task-grids", "rc-driver");
  fs.mkdirSync(path.dirname(out), { recursive: true });
  const b = spawnSync("go", ["build", "-o", out, "."], {
    cwd: path.join(HERE, "driver"),
    encoding: "utf8",
  });
  if (b.status !== 0) {
    throw new Error(`the driver did not build: ${b.stdout ?? ""}${b.stderr ?? ""}`);
  }
  driverBinPath = out;
  return out;
}

/** The marker check 4 types, and the budget it must come back inside. */
const MARKER = "MAGMUX_RC_DEMO_OK";
const MARKER_BUDGET_MS = 1000;
/** How long check 6 gives the client to notice magmux is gone. */
const DEATH_BUDGET_MS = 5000;

/**
 * Wait until a pane has produced no frame for `quietMs`.
 *
 * A POLL on the pane going quiet, never a fixed sleep: a sleep long enough for
 * a loaded laptop is wasted on every other run, and a sleep sized to an idle
 * one measures the shell's first prompt arriving late instead of the thing
 * under test.
 */
async function paneQuiet(
  ws: { messages: { ev: any }[] },
  pane: number,
  quietMs: number,
  within: number,
): Promise<void> {
  const deadline = Date.now() + within;
  let last = -1;
  let lastChange = Date.now();
  while (Date.now() < deadline) {
    const n = ws.messages.filter((x) => x.ev.type === "frame" && x.ev.pane === pane).length;
    if (n !== last) {
      last = n;
      lastChange = Date.now();
    } else if (Date.now() - lastChange >= quietMs) return;
    await sleep(50);
  }
}

// ── check 1: applyFrame over a keyframe and three deltas ────────────────────

console.log(`\n${C.bold}  1 · the decoder, on hand-built frames${C.reset}`);

function frame(over: Partial<FrameMessage>): FrameMessage {
  return {
    type: "frame",
    pane: 0,
    seq: 1,
    rows: 4,
    cols: 10,
    alt: false,
    sb: 0,
    scrolled: false,
    cur: { y: 0, x: 0, vis: true },
    key: false,
    ...over,
  };
}

{
  const screen = newScreen(0, 0);

  // A keyframe covering every row, with one styled run: bold red on default
  // over "alpha", and a run that extends PAST the text, which is how a styled
  // blank survives the right-trim.
  const keyChanged = applyFrame(
    screen,
    frame({
      key: true,
      seq: 7,
      cur: { y: 1, x: 3, vis: true },
      lines: [
        { y: 0, t: "alpha", r: [[0, 5, 1, -1, ATTR_BOLD]] },
        { y: 1, t: "beta" },
        { y: 2, t: "" },
        { y: 3, t: "gamma", r: [[0, 10, -1, 4, 0]] },
      ],
    }),
  );
  check(
    "a keyframe returns every row index and sets the cleared flag",
    keyChanged.length === 4 && keyChanged.join(",") === "0,1,2,3" && screen.cleared === true,
    `changed=[${keyChanged}] cleared=${screen.cleared} rows=${screen.rows} cols=${screen.cols}`,
  );

  // Three deltas, each replacing WHOLE rows. Row 1 is replaced twice, so the
  // last write wins — which is the property that makes a coalesced frame and a
  // sequence of frames come out identical.
  const d1 = applyFrame(screen, frame({ seq: 8, lines: [{ y: 1, t: "BETA" }] }));
  const d2 = applyFrame(screen, frame({ seq: 9, lines: [{ y: 2, t: "delta", r: [[2, 3, 46, -1, ATTR_UNDERLINE]] }] }));
  const d3 = applyFrame(
    screen,
    frame({ seq: 10, cur: { y: 2, x: 5, vis: false }, lines: [{ y: 1, t: "beta!" }] }),
  );

  check(
    "a delta returns only the rows it carried, and never clears",
    d1.join(",") === "1" && d2.join(",") === "2" && d3.join(",") === "1" && screen.cleared === false,
    `d1=[${d1}] d2=[${d2}] d3=[${d3}] cleared=${screen.cleared}`,
  );

  const want = ["alpha", "beta!", "delta", "gamma"].join("\n");
  check(
    "the merged text is the keyframe with each delta's row replaced",
    screenText(screen) === want,
    `${JSON.stringify(screenText(screen))} want ${JSON.stringify(want)}`,
  );

  check(
    "the cursor takes the newest frame's value, hidden included",
    screen.cur.y === 2 && screen.cur.x === 5 && screen.cur.vis === false && screen.seq === 10,
    `cur=${JSON.stringify(screen.cur)} seq=${screen.seq}`,
  );

  // Spans: the styled run, the unstyled remainder, and the run that runs past
  // the text into the trimmed blanks.
  const row0 = rowSpans(screen.lines[0], cellCols(screen.lines[0]), screen.cols);
  const styled = row0[0];
  check(
    "a run decodes to a span in cell columns with its colours and attributes",
    row0.length === 2 &&
      styled.col === 0 &&
      styled.width === 5 &&
      styled.text === "alpha" &&
      styled.fg === 1 &&
      styled.bg === -1 &&
      styled.attr === ATTR_BOLD &&
      row0[1].col === 5 &&
      row0[1].width === 5 &&
      row0[1].text === "     ",
    `${row0.length} spans: ${JSON.stringify(row0)}`,
  );

  const row3 = rowSpans(screen.lines[3], cellCols(screen.lines[3]), screen.cols);
  check(
    "a run past the end of the text keeps the styled blanks alive",
    row3.length === 1 && row3[0].width === 10 && row3[0].bg === 4 && row3[0].text === "gamma     ",
    `${row3.length} spans, first ${JSON.stringify(row3[0])}`,
  );

  // A header-only frame: no lines at all, and the cursor moves. A client that
  // discarded it would lose the caret.
  const none = applyFrame(screen, frame({ seq: 11, cur: { y: 0, x: 9, vis: true } }));
  check(
    "a frame with no lines is still a frame: the cursor follows it",
    none.length === 0 && screen.cur.x === 9 && screen.cur.vis === true && screen.seq === 11,
    `changed=[${none}] cur=${JSON.stringify(screen.cur)}`,
  );

  // A geometry change forces a repaint even though the frame does not say key.
  // Geometry rides on EVERY frame header, not only on the watch reply: a pane
  // reflows whenever the human resizes the window.
  const resized = applyFrame(screen, frame({ seq: 12, rows: 6, cols: 20, lines: [{ y: 5, t: "six" }] }));
  check(
    "a geometry change is treated as a keyframe, because row deltas cannot cross one",
    resized.length === 6 && screen.cleared === true && screen.rows === 6 && screen.cols === 20,
    `changed=${resized.length} rows=${screen.rows} cols=${screen.cols} cleared=${screen.cleared}`,
  );

  // An alt-screen switch, likewise: a delta from the primary screen applied on
  // top of the alternate one would be a blend of two different screens.
  const toAlt = applyFrame(
    screen,
    frame({ seq: 13, rows: 6, cols: 20, alt: true, lines: [{ y: 0, t: "vim" }] }),
  );
  check(
    "an alt-screen switch is treated as a keyframe too, and is reported in the header",
    toAlt.length === 6 && screen.cleared === true && screen.alt === true,
    `changed=${toAlt.length} alt=${screen.alt} cleared=${screen.cleared}`,
  );
}

// Colours and the nine attribute bits. The bits are `mux/cell.go:10-18`,
// restated in a demo that is not allowed to import that package; this is what
// catches a renumbering before a human catches it in a screenshot.
{
  const c = classifyColour;
  const trueColour = c((1 << 24) | 0x336699);
  check(
    "colour classification: -1 default, 0..255 indexed, >= 1<<24 truecolor",
    c(-1).kind === "default" &&
      c(4).kind === "indexed" &&
      (c(4) as any).index === 4 &&
      c(255).kind === "indexed" &&
      trueColour.kind === "rgb" &&
      (trueColour as any).r === 0x33 &&
      (trueColour as any).g === 0x66 &&
      (trueColour as any).b === 0x99,
    `-1→default, 4→indexed 4, 0x1336699→${JSON.stringify(trueColour)}`,
  );
  const bits = [
    ATTR_BOLD,
    ATTR_DIM,
    ATTR_ITALIC,
    ATTR_BLINK,
    ATTR_REVERSE,
    ATTR_INVIS,
    ATTR_UNDERLINE,
    ATTR_STRIKE,
    ATTR_OVERLINE,
  ];
  check(
    "the nine attribute bits still match mux/cell.go:10-18",
    bits.join(",") === "1,2,4,8,16,32,64,128,256",
    `[${bits.join(",")}]`,
  );
}

// ── check 2: double-width, both directions ──────────────────────────────────

console.log(`\n${C.bold}  2 · double-width glyphs map cells to code points both ways${C.reset}`);

{
  // "a漢b字c": two wide glyphs at code-point indexes 1 and 3, so the row is
  // seven CELLS wide and five code points long.
  const line = { y: 0, t: "a漢b字c", wd: [1, 3] };
  const cells = cellCols(line);
  const colOf = Array.from(cells.colOf);
  const cpAt = Array.from(cells.cpAt);
  check(
    "cp → cell: each code point starts where the wide ones have pushed it",
    colOf.join(",") === "0,1,3,4,6,7",
    `colOf=[${colOf.join(",")}] want [0,1,3,4,6,7]`,
  );
  check(
    "cell → cp: the continuation half of a wide glyph is -1, not a repeat",
    cpAt.join(",") === "0,1,-1,2,3,-1,4",
    `cpAt=[${cpAt.join(",")}] want [0,1,-1,2,3,-1,4]`,
  );

  // The point of all of it: a run at CELL column 4 must land on the second
  // wide glyph, not on the fourth character.
  const spans = rowSpans({ ...line, r: [[4, 2, 2, -1, 0]] }, cells, 9);
  const styled = spans.find((s) => s.fg === 2);
  check(
    "a run in cell columns lands on the glyph it names, not one character right",
    styled?.col === 4 && styled?.text === "字" && styled?.width === 2,
    `styled span ${JSON.stringify(styled)}`,
  );
  const total = spans.reduce((n, s) => n + s.width, 0);
  check(
    "every cell column of the row is covered, blanks past the text included",
    total === 9,
    `${spans.length} spans covering ${total} of 9 columns`,
  );
}

// ── the live half: a real binary, a real static server ──────────────────────

const bin = buildMagmux();
const m = new RemoteMagmux(bin, "demo-rc");
const viewTokenFile = path.join(m.dir, "view.token");
const urlFile = path.join(m.dir, "magmux.url");
let serve: ChildProcess | null = null;
let serveOut = "";
let ws: ReturnType<RemoteMagmux["ws"]> | null = null;
/** Check 4's second connection: the FULL token, the one that may type. */
let full: ReturnType<RemoteMagmux["ws"]> | null = null;
/** Check 6's subject: the demo's own CLI client, as a separate process. */
let cli: ChildProcess | null = null;

try {
  // ── 7 (part one): the static server starts FIRST, because magmux's
  //    --allow-origin must name a port the kernel has not chosen yet.
  // RC_DEMO_WEB_DRIVER is PINNED on rather than inherited, for the same reason
  // checks 9 and 10 pin the tmux mode: check 14 is the endpoint's own checks,
  // and an operator who had switched the driver off in their shell would
  // otherwise measure the wrong server. Check 14 starts its own `=0` one.
  serve = spawn("bun", [path.join(HERE, "serve.ts"), "--state", m.dir], {
    stdio: ["ignore", "pipe", "pipe"],
    env: { ...process.env, RC_DEMO_WEB_DRIVER: "1" },
  });
  serve.stdout?.on("data", (b) => {
    serveOut += b.toString();
  });
  serve.stderr?.on("data", (b) => {
    serveOut += b.toString();
  });
  await until("serve.ts to print its ready line", 30_000, () => /^ready \{/m.test(serveOut));
  const ready = JSON.parse(/^ready (\{.*\})$/m.exec(serveOut)![1]);
  const webOrigin = `http://127.0.0.1:${ready.port}`;
  const configURL = webOrigin + ready.configPath;
  /** What a browser sends on a same-origin fetch, and what curl does not. */
  const asPage = { "Sec-Fetch-Site": "same-origin" };

  console.log(`\n${C.bold}  Setup${C.reset}`);
  console.log(`  ${C.grey}static server ${C.reset}${webOrigin}`);
  console.log(`  ${C.grey}config path   ${C.reset}${ready.configPath}`);
  console.log(`  ${C.grey}view token    ${C.reset}${ready.viewTokenFile}`);

  const mode = fs.statSync(viewTokenFile).mode & 0o777;
  check(
    "the view token is minted before the bind, at 0600",
    mode === 0o600 && ready.viewTokenFile === viewTokenFile,
    `mode 0${mode.toString(8)} at ${ready.viewTokenFile}`,
  );
  check(
    "the config lives at an unguessable per-run path, and the fixed one is a 404",
    /^\/c\/[A-Za-z0-9_-]{32}\.json$/.test(ready.configPath ?? "") &&
      (await fetch(`${webOrigin}/config.json`, { headers: asPage })).status === 404,
    `configPath=${ready.configPath}`,
  );

  // The 503, BEFORE magmux exists. This server binds first by necessity — its
  // port has to be in magmux's --allow-origin — so there is a real window in
  // which it cannot answer, and a legible error is what stops the page falling
  // back to its own origin and looking like an auth failure instead.
  const early = await fetch(configURL, { headers: asPage });
  const earlyBody = await early.json();
  check(
    "before magmux has bound, the config is a 503 that says so",
    early.status === 503 && String(earlyBody?.error).includes("magmux has not bound"),
    `${early.status} ${JSON.stringify(earlyBody)}`,
  );

  // magmux gets the token serve.ts minted and the origin serve.ts is on. These
  // are the exact flags demo.sh passes, so a typo fails a check rather than a
  // demo.
  await m.start([
    "-e",
    "sh",
    "--label",
    "shell",
    "--view-token-file",
    viewTokenFile,
    "--allow-origin",
    webOrigin,
    "--allow-origin",
    `http://localhost:${ready.port}`,
  ]);
  m.viewToken = fs.readFileSync(viewTokenFile, "utf8").trim();
  fs.writeFileSync(urlFile, m.base + "\n");
  console.log(`  ${C.grey}magmux        ${C.reset}${m.base}`);

  // ── check 7 (part two): what the page is actually served ────────────────
  console.log(`\n${C.bold}  7 · the static server${C.reset}`);

  const curlLike = await fetch(configURL);
  check(
    "a request with no Sec-Fetch-Site and no self Origin — a curl from another shell — is refused",
    curlLike.status === 403,
    `${curlLike.status} ${(await curlLike.text()).trim().slice(0, 80)}`,
  );

  const cfgRes = await fetch(configURL, { headers: asPage });
  const cfg = await cfgRes.json();
  check(
    "the page's own same-origin request gets the view token and magmux's URL",
    cfgRes.status === 200 && cfg.viewToken === m.viewToken && cfg.magmux === m.base,
    `status ${cfgRes.status} magmux=${cfg.magmux} token=${String(cfg.viewToken).length} chars`,
  );
  check(
    "the token is 43 characters of magmux's own alphabet (auth.LoadFile refuses anything else)",
    typeof cfg.viewToken === "string" && cfg.viewToken.length === 43 && /^[A-Za-z0-9_-]{43}$/.test(cfg.viewToken),
    `${String(cfg.viewToken).length} chars, ${/^[A-Za-z0-9_-]+$/.test(cfg.viewToken) ? "base64url" : "NOT base64url"}`,
  );
  check(
    "the config sets no Access-Control-Allow-Origin, so another origin cannot read the credential",
    cfgRes.headers.get("access-control-allow-origin") === null,
    `Access-Control-Allow-Origin: ${cfgRes.headers.get("access-control-allow-origin") ?? "(absent)"}`,
  );

  const appRes = await fetch(`${webOrigin}/app.js`, { headers: asPage });
  const appJS = await appRes.text();
  check(
    "/app.js is a real bundle of the SHARED source, not a copy",
    appRes.status === 200 &&
      appJS.length > 2_000 &&
      appJS.includes("rowSpans") &&
      appJS.includes("magmux.auth.") &&
      appJS.includes("cpAt"),
    `${appJS.length} bytes; rowSpans=${appJS.includes("rowSpans")} subprotocol=${appJS.includes("magmux.auth.")}`,
  );

  // ── check 8: the Origin rules the demo's flags depend on ────────────────
  console.log(`\n${C.bold}  8 · Origin, as --allow-origin decides it${C.reset}`);

  const evil = await m.http("GET", "/v1/capabilities", {
    token: m.viewToken,
    headers: { Origin: "http://evil.example" },
  });
  check(
    "an origin magmux was not told about is refused 403, naming the flag that would permit it",
    evil.status === 403 && evil.json?.code === "forbidden" && String(evil.json?.error).includes("--allow-origin"),
    `${evil.status} ${evil.json?.code} — ${String(evil.json?.error).slice(0, 90)}`,
  );

  const mine = await m.http("GET", "/v1/capabilities", {
    token: m.viewToken,
    headers: { Origin: webOrigin },
  });
  check(
    "the demo's own origin is allowed and comes back with the CORS header the browser needs",
    mine.status === 200 && mine.headers.get("access-control-allow-origin") === webOrigin && mine.json?.readOnly === true,
    `${mine.status} ACAO=${mine.headers.get("access-control-allow-origin")} readOnly=${mine.json?.readOnly}`,
  );

  // ── check 3: the message sequence, against the real binary ──────────────
  console.log(`\n${C.bold}  3 · the WebSocket sequence with the view token${C.reset}`);

  ws = m.ws(m.viewToken);
  await ws.open();

  const agg = await ws.seen((ev) => ev.type === "snapshot" && Array.isArray(ev.panes), 15_000, "the aggregate");
  check(
    "the aggregate is the first message on the connection",
    ws.messages[0]?.ev?.type === "snapshot" && Array.isArray(ws.messages[0]?.ev?.panes),
    `first message type=${ws.messages[0]?.ev?.type}, ${agg.ev.panes.length} panes`,
  );

  let pane = -1;
  for (const p of agg.ev.panes ?? []) {
    if (p?.state === "panel") continue;
    if (typeof p?.pane === "number") {
      pane = p.pane;
      break;
    }
  }

  const hello = await ws.call("hello", { client: "magmux-rc-demo/selftest" });
  check(
    "`hello` proves the credential is the READ-ONLY one",
    hello.readOnly === true,
    `readOnly=${hello.readOnly} protocol=${hello.protocol} transport=${hello.transport}`,
  );

  const info = await ws.call("watch", { pane, mode: "frames", fps: 15 });
  check(
    "the watch reply carries the geometry a client sizes itself from",
    info.pane === pane && info.rows > 0 && info.cols > 0 && info.mode === "frames",
    `pane=${info.pane} ${info.rows}x${info.cols} mode=${info.mode} fps=${info.fps}`,
  );

  const key = await ws.seen((ev) => ev.type === "frame" && ev.pane === pane && ev.key === true, 20_000, "a keyframe");
  check(
    "the first frame is a keyframe covering every row",
    key.ev.key === true && key.ev.lines.length === key.ev.rows && key.ev.rows === info.rows,
    `seq=${key.ev.seq} lines=${key.ev.lines.length}/${key.ev.rows}`,
  );

  const watchIdx = ws.messages.findIndex((x) => x.ev.type === "reply" && x.ev.result?.rows);
  const frameIdx = ws.messages.findIndex((x) => x.ev.type === "frame");
  check(
    "no frame precedes the watch reply that created it",
    watchIdx >= 0 && frameIdx > watchIdx,
    `watch reply at #${watchIdx}, first frame at #${frameIdx}`,
  );

  // The decoder on a REAL frame: the whole reason this file exists.
  const live = newScreen(info.rows, info.cols, pane);
  const changed = applyFrame(live, key.ev as FrameMessage);
  const painted = live.lines.reduce(
    (n, l) => n + rowSpans(l, cellCols(l), live.cols).reduce((w, s) => w + s.width, 0),
    0,
  );
  check(
    "the shared decoder applies a real keyframe and covers every cell of it",
    changed.length === info.rows && painted === info.rows * info.cols,
    `${changed.length} rows, ${painted} cells painted of ${info.rows * info.cols}`,
  );

  // ── check 4: one pane, two credentials ──────────────────────────────────
  //
  // THE DEMO'S CENTRAL CLAIM, MEASURED. A second connection holding the FULL
  // token types into the pane; the READ-ONLY connection already watching it
  // sees the result. There is one hub, one framer and one shadow behind both,
  // so what this really measures is the path a human's keystroke takes to a
  // browser on the other side of the room:
  //
  //   input → PTY → readLoop → noteOutputLocked → the pane's framer
  //   → one diff, one encode → each subscriber's latest-wins slot → here.
  //
  // The budget is asserted rather than merely observed, and the number is
  // printed, because a "did a frame ever arrive" test still passes against a
  // framer that has stopped being wake-driven and fallen back to something slow.
  console.log(`\n${C.bold}  4 · a FULL-token client types; the VIEW-token client sees it${C.reset}`);

  full = m.ws(m.token);
  await full.open();
  const fullHello = await full.call("hello", { client: "magmux-rc-demo/selftest-full" });

  // Let the shell settle on its first prompt, so the frame measured below is
  // the one this marker produced and not the prompt arriving late.
  await paneQuiet(ws, pane, 400, 15_000);

  const framesBefore = ws.messages.filter((x) => x.ev.type === "frame" && x.ev.pane === pane).length;
  const typedAt = Date.now();
  const inputReply = await full.call("input", { pane, text: `echo ${MARKER}\r` });

  // The marker must be on a line of its OWN. The terminal echoes the command
  // line too, and an echo is not evidence that the shell ran anything.
  const arrived = await ws.await(
    (ev) =>
      ev.type === "frame" &&
      ev.pane === pane &&
      frameText(ev)
        .split("\n")
        .some((l: string) => l.trim() === MARKER),
    20_000,
    `a frame carrying ${MARKER} on a line of its own`,
  );
  const latencyMs = arrived.at - typedAt;

  console.log(`\n${C.bold}  Measured${C.reset}`);
  console.log(
    `  ${C.grey}input acknowledged${C.reset}  ${inputReply.bytes} bytes reached pane ${inputReply.pane}'s PTY`,
  );
  console.log(
    `  ${C.grey}marker on the view${C.reset}  ${latencyMs}ms  ${C.grey}(budget ${MARKER_BUDGET_MS}ms)${C.reset}`,
  );

  check(
    `the read-only client saw the full-token client's ${MARKER} within ${MARKER_BUDGET_MS}ms`,
    latencyMs <= MARKER_BUDGET_MS && fullHello.readOnly === false,
    `${latencyMs}ms; the typing connection was readOnly=${fullHello.readOnly}, the watching one readOnly=${hello.readOnly}`,
  );
  check(
    "it arrived as a DELTA on the live stream, not as a fresh keyframe",
    arrived.ev.key === false && arrived.ev.lines.length < arrived.ev.rows,
    `key=${arrived.ev.key} ${arrived.ev.lines.length} of ${arrived.ev.rows} rows, seq=${arrived.ev.seq}, ` +
      `${framesBefore} frames before the marker`,
  );

  // And now through the SHARED DECODER, which is the part a human would
  // otherwise be the first to check: replay every frame this connection
  // received, keyframe then deltas, and read the merged screen.
  const replay = newScreen(0, 0, pane);
  for (const rec of ws.messages) {
    if (rec.ev.type === "frame" && rec.ev.pane === pane) applyFrame(replay, rec.ev as FrameMessage);
  }
  const markerRow = screenText(replay)
    .split("\n")
    .findIndex((l) => l.trim() === MARKER);
  check(
    "and the shared decoder, replaying keyframe + deltas, puts it on a line of its own",
    markerRow >= 0,
    markerRow >= 0
      ? `row ${markerRow} of the merged ${replay.rows}x${replay.cols} screen, after ${replay.frames} frames`
      : `no row of the merged screen is exactly ${MARKER}`,
  );

  // ── check 5: the view token cannot type ─────────────────────────────────
  console.log(`\n${C.bold}  5 · what the view token is refused${C.reset}`);

  const inputID = ws.send("input", { pane, text: "echo NOPE\r" });
  const refusal = await ws.seen(
    (ev) => ev.type === "reply" && String(ev.id) === String(inputID),
    10_000,
    "the reply to input",
  );
  check(
    "`input` from the view token is refused `forbidden`, so the page cannot type into a shell",
    refusal.ev.ok === false && refusal.ev.code === "forbidden",
    `ok=${refusal.ev.ok} code=${refusal.ev.code} — ${String(refusal.ev.error).slice(0, 70)}`,
  );

  const sendRefusal = await m.http("POST", "/v1/ops/send", {
    token: m.viewToken,
    body: { pane, text: "hi" },
  });
  check(
    "and so is `send` over REST — the refusal is not a property of one transport",
    sendRefusal.status === 403 && sendRefusal.json?.code === "forbidden",
    `${sendRefusal.status} ${sendRefusal.json?.code}`,
  );

  // ── check 14: the driver endpoint the browser panel presses ─────────────
  //
  // THE ESCALATION, TESTED RATHER THAN DESCRIBED. The page holds the read-only
  // view token and its whole security claim rests on that, so the driver's
  // actions are performed by `serve.ts` — which holds both tokens — behind an
  // unguessable per-run path and a `Sec-Fetch-Site: same-origin` gate. Every
  // property that keeps that honest is checked here: the gate, the path, the
  // allow-list refusing an action of its own accord, magmux's real answer
  // arriving unchanged, and RC_DEMO_WEB_DRIVER=0 removing the endpoint.
  console.log(`\n${C.bold}  14 · the browser page's driver endpoint${C.reset}`);

  const driverURL = webOrigin + ready.driverPath;
  check(
    "the driver lives at its own unguessable per-run path, separate from the config's",
    /^\/d\/[A-Za-z0-9_-]{32}\.json$/.test(ready.driverPath ?? "") && ready.driverPath !== ready.configPath,
    `driverPath=${ready.driverPath}`,
  );

  const drvCurl = await fetch(driverURL, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ action: "type", pane }),
  });
  const drvCurlBody = await drvCurl.json().catch(() => null);
  check(
    "without Sec-Fetch-Site: same-origin — a curl from another shell — it is refused 403",
    drvCurl.status === 403 && drvCurlBody?.by === "serve.ts",
    `${drvCurl.status} ${JSON.stringify(drvCurlBody).slice(0, 90)}`,
  );

  const guessed = await fetch(`${webOrigin}/d/${"A".repeat(32)}.json`, {
    method: "POST",
    headers: { ...asPage, "Content-Type": "application/json" },
    body: JSON.stringify({ action: "type", pane }),
  });
  check(
    "a GUESSED driver path is a 404, even from the page's own origin",
    guessed.status === 404,
    `${guessed.status} at /d/AAAA…json`,
  );

  const listed = await fetch(driverURL, { headers: asPage });
  const listedBody: any = await listed.json();
  const names: string[] = (listedBody?.actions ?? []).map((a: any) => a.name);
  check(
    "the page builds its panel FROM the allow-list, so it cannot show a button that would be refused",
    listed.status === 200 && names.includes("type") && names.includes("refuse-input") && names.length >= 10,
    `${names.length} actions: ${names.join(", ")}`,
  );

  // A permitted action, and magmux's REAL answer. The marker goes through the
  // same path a human's keystroke does, so this also proves the endpoint is
  // spending the session token rather than the page's.
  const typedAt2 = Date.now();
  const drvType = await fetch(driverURL, {
    method: "POST",
    headers: { ...asPage, "Content-Type": "application/json" },
    body: JSON.stringify({ action: "type", pane }),
  });
  const drvTypeBody: any = await drvType.json();
  check(
    "a permitted action returns magmux's OWN status and body, unchanged",
    drvType.status === 200 &&
      drvTypeBody?.status === 200 &&
      /"ok":\s*true/.test(String(drvTypeBody?.text)) &&
      drvTypeBody?.request?.path === "/v1/ops/input" &&
      drvTypeBody?.request?.cred === "session" &&
      typeof drvTypeBody?.ms === "number",
    `HTTP ${drvType.status}, upstream ${drvTypeBody?.status} in ${drvTypeBody?.ms}ms — ${String(
      drvTypeBody?.text,
    ).trim().slice(0, 70)}`,
  );
  const typedArrived = await ws.await(
    (ev) => ev.type === "frame" && ev.pane === pane && /ls --color=auto/.test(frameText(ev)),
    10_000,
    "a frame carrying the command the driver endpoint typed",
  );
  check(
    "and it really typed: the read-only client saw the command on the live stream",
    typedArrived.at - typedAt2 < 10_000,
    `${typedArrived.at - typedAt2}ms after the POST, seq=${typedArrived.ev.seq}`,
  );

  // Item 8, through the browser's own driver: magmux's 403, verbatim, with the
  // status preserved so the panel shows the real refusal rather than an
  // invented one.
  const drvRefuse = await fetch(driverURL, {
    method: "POST",
    headers: { ...asPage, "Content-Type": "application/json" },
    body: JSON.stringify({ action: "refuse-input", pane }),
  });
  const drvRefuseBody: any = await drvRefuse.json();
  check(
    "the VIEW-token action comes back as magmux's real 403 forbidden, status and all",
    drvRefuse.status === 403 &&
      drvRefuseBody?.status === 403 &&
      /"code":\s*"forbidden"/.test(String(drvRefuseBody?.text)) &&
      drvRefuseBody?.request?.cred === "view" &&
      drvRefuseBody?.by === undefined,
    `HTTP ${drvRefuse.status} — ${String(drvRefuseBody?.text).trim().slice(0, 90)}`,
  );

  const drvUnknown = await fetch(driverURL, {
    method: "POST",
    headers: { ...asPage, "Content-Type": "application/json" },
    body: JSON.stringify({ action: "input", pane, text: "rm -rf /" }),
  });
  const drvUnknownBody: any = await drvUnknown.json();
  check(
    "an action NOT on the allow-list is refused by serve.ts ITSELF — it is a list, not a proxy",
    drvUnknown.status === 400 &&
      drvUnknownBody?.by === "serve.ts" &&
      Array.isArray(drvUnknownBody?.allowed) &&
      drvUnknownBody?.status === undefined,
    `${drvUnknown.status} by=${drvUnknownBody?.by} — ${String(drvUnknownBody?.error).slice(0, 80)}`,
  );

  // RC_DEMO_WEB_DRIVER=0: a SECOND serve.ts, against the same state directory,
  // with the switch off. The endpoint must not exist at all — no path in the
  // ready line, no `driver` in the config, and nothing at the path the first
  // server minted.
  const offProc = spawn("bun", [path.join(HERE, "serve.ts"), "--state", m.dir, "--token-file", path.join(m.dir, "off.token")], {
    stdio: ["ignore", "pipe", "pipe"],
    env: { ...process.env, RC_DEMO_WEB_DRIVER: "0" },
  });
  let offOut = "";
  offProc.stdout?.on("data", (b) => (offOut += b.toString()));
  offProc.stderr?.on("data", (b) => (offOut += b.toString()));
  try {
    await until("the RC_DEMO_WEB_DRIVER=0 server to print its ready line", 30_000, () => /^ready \{/m.test(offOut));
    const offReady = JSON.parse(/^ready (\{.*\})$/m.exec(offOut)![1]);
    const offOrigin = `http://127.0.0.1:${offReady.port}`;
    const offPage = { "Sec-Fetch-Site": "same-origin" };
    const offCfg = await (await fetch(offOrigin + offReady.configPath, { headers: offPage })).json();
    const offHit = await fetch(offOrigin + ready.driverPath, {
      method: "POST",
      headers: { ...offPage, "Content-Type": "application/json" },
      body: JSON.stringify({ action: "type", pane }),
    });
    check(
      "RC_DEMO_WEB_DRIVER=0: no path is minted, the config carries no driver, and the route is a 404",
      offReady.driverPath === undefined && offCfg?.driver === null && offHit.status === 404,
      `driverPath=${offReady.driverPath ?? "(absent)"} config.driver=${JSON.stringify(offCfg?.driver)} route=${offHit.status}`,
    );
  } finally {
    offProc.kill("SIGTERM");
    fs.rmSync(path.join(m.dir, "off.token"), { force: true });
  }

  // ── check 6: magmux dies, and the client says so ────────────────────────
  //
  // CRITERION 8, AUTOMATED, AND IT MUST BE LAST: it kills the binary every
  // check above is using.
  //
  // The subject is the REAL `demo/rc/cli.ts` as a separate process, not the
  // core called in-band, because "exits non-zero" is a property of the
  // program a human runs and the exit code is the only part of this that a
  // launcher can act on.
  //
  // SIGKILL, not SIGTERM, and the difference is the whole design rather than a
  // convenience. A SIGTERM'd magmux tears its subscribers down in order —
  // `results`, then `shutdown`, then EOF — and the client reports an ORDERLY
  // END and exits 0, which is correct and is not what criterion 8 is about.
  // Criterion 8 is the machine going away: the socket closes with no
  // `shutdown` in front of it, and the one failure this demo must never have
  // is a client left showing a still picture of a magmux that is gone.
  console.log(`\n${C.bold}  6 · when magmux is killed, the client says so and exits non-zero${C.reset}`);

  cli = spawn(
    "bun",
    [path.join(HERE, "cli.ts"), "--url", m.base, "--token-file", viewTokenFile, "--verbose"],
    { stdio: ["ignore", "pipe", "pipe"], env: { ...process.env, COLUMNS: "100", LINES: "40" } },
  );
  let cliErr = "";
  let cliExit: number | null = null;
  let cliSignal: string | null = null;
  let cliExitAt = 0;
  // The ANSI painter writes a whole screen to stdout; an undrained pipe would
  // fill and block it in exactly the place this check is trying to measure.
  cli.stdout?.on("data", () => {});
  cli.stderr?.on("data", (b) => {
    cliErr += b.toString();
  });
  cli.on("exit", (code, sig) => {
    cliExit = code;
    cliSignal = sig;
    cliExitAt = Date.now();
  });

  // Wait until it has a PICTURE, not merely a connection: the watch reply AND
  // the first frame. Killing magmux before a frame has landed would test a
  // client that failed to start, and this check is about one that was working.
  await until("the demo's own CLI client to paint its first frame", 30_000, () =>
    /watch reply: pane=/.test(cliErr) && /first frame \(watch reply was/.test(cliErr),
  );

  const killedAt = Date.now();
  m.proc?.kill("SIGKILL");
  await until(`the CLI client to exit within ${DEATH_BUDGET_MS}ms`, DEATH_BUDGET_MS + 5_000, () => cliExit !== null || cliSignal !== null);
  const noticedMs = cliExitAt - killedAt;
  const lastLine = cliErr.trim().split("\n").pop() ?? "";
  // Strip the colour the painter wraps its final line in.
  const reason = lastLine.replace(/\x1b\[[0-9;]*m/g, "").trim();

  console.log(`\n${C.bold}  Measured${C.reset}`);
  console.log(
    `  ${C.grey}noticed after     ${C.reset}${noticedMs}ms  ${C.grey}(budget ${DEATH_BUDGET_MS}ms)${C.reset}`,
  );
  console.log(`  ${C.grey}said              ${C.reset}${reason}`);

  check(
    `the client noticed magmux was gone within ${DEATH_BUDGET_MS}ms and said why`,
    noticedMs <= DEATH_BUDGET_MS && /magmux-rc:/.test(reason) && reason.length > "magmux-rc:".length + 5,
    `${noticedMs}ms, and it printed: ${JSON.stringify(reason.slice(0, 110))}`,
  );
  check(
    "and it EXITED non-zero, so a launcher can tell a dead mirror from a quiet one",
    cliExit !== 0,
    `exit code ${cliExit}${cliSignal ? ` (signal ${cliSignal})` : ""} — 0 would mean an orderly end, which this was not`,
  );
} catch (err: any) {
  check("the selftest ran to completion", false, String(err?.message ?? err));
  if (m.hasExited || m.stderr) dumpStderr(m);
  if (serveOut) console.log(`\n  ${C.bold}serve.ts output${C.reset}\n  ${C.grey}${serveOut.trim()}${C.reset}`);
} finally {
  ws?.close();
  full?.close();
  // Check 6 kills magmux, so its CLI client is normally already gone; killing
  // it anyway is what keeps a FAILING run from leaving a bun watching a socket.
  cli?.kill("SIGKILL");
  if (serve) {
    serve.kill("SIGTERM");
  }
  await m.stop();
  m.cleanup();
}

// ── check 9: the launcher itself, end to end ────────────────────────────────
//
// G1 in the test plan, and the highest-value check that was missing from this
// file: `demo/rc/demo.sh` is the one file nothing ever executed. A typo in a
// flag, a reordered startup, a broken readiness poll or a teardown that leaks
// breaks the whole demo on its first run with all 38 checks above green.
//
// It runs the launcher in the shape the launcher itself documents for exactly
// this — NO_BROWSER=1 NO_ATTACH=1 — so the demo is built in full, nothing
// attaches, and the launcher process still owns the teardown. RC_DEMO_ID is
// derived PER RUN, so this can collide neither with a demo a human is running
// nor with a second copy of itself. RC_DEMO_STATE is forced under /tmp because
// a worktree's own path plus two copies of that id can overrun the 100-byte
// socket limit demo.sh refuses at (sockdir/sockdir.go: sunPathMax).
//
// It is LAST for the same reason check 6 is last within the block above: it is
// the only check that starts a second magmux, a second static server and a
// tmux server, and it must be the thing that cleans them up.
console.log(`\n${C.bold}  9 · the launcher: demo.sh end to end, and its teardown${C.reset}`);

/**
 * What pane A prints and pane B must mirror.
 *
 * A row LABEL from the lower half of the colour card, not its title. The title
 * used to be the marker, and with the driver taking a third of the window the
 * magmux pane is ~16 rows against a ~16-row card: the title scrolls off before
 * anything can capture it, while the picture underneath it is perfectly
 * correct. A marker that a correct demo fails is worse than no marker.
 */
const CARD_MARKER = "truecolor";
/** A line only the driver's menu prints, so pane C cannot be confused with a mirror. */
const DRIVER_MARKER = "type a command";
/** How long demo.sh gets to build everything and print its NO_ATTACH banner. */
const LAUNCH_BUDGET_MS = 90_000;
/** How long each pane gets to paint the card once the banner is out. */
const PAINT_BUDGET_MS = 30_000;
/**
 * How long the session must survive UNATTACHED before that counts as proof.
 *
 * MEASURED, and the failure it guards is invisible in code review:
 * `set-option destroy-unattached on` beside `new-session -d` destroys the
 * session AND the whole tmux server within about THREE seconds, before
 * anything has attached (tmux 3.7c; the transcript is in demo.sh, beside the
 * `client-attached` hook that fixes it). Five seconds is comfortably past that
 * window, and the elapsed time is printed so a shrinking margin is visible
 * rather than merely survived.
 */
const UNATTACHED_PROOF_MS = 5_000;

function tmuxVersion(): string | null {
  try {
    return execFileSync("tmux", ["-V"], { stdio: ["ignore", "pipe", "ignore"] })
      .toString()
      .trim();
  } catch {
    return null;
  }
}

const tmuxV = tmuxVersion();
if (!tmuxV) {
  // SKIP, and say why: this is the only check with a dependency the other 38
  // do not have, and a machine without tmux has not broken the demo — it
  // merely cannot run it, which is what demo.sh's own preflight says too.
  console.log(
    `  ${C.yellow}SKIPPED${C.reset} ${C.grey}— tmux is not installed, and the demo IS a tmux layout ` +
      `(brew install tmux). The ${checks.length} checks above are unaffected.${C.reset}`,
  );
} else {
  const runID = `smoke-${process.pid}-${crypto.randomBytes(3).toString("hex")}`;
  const runState = `/tmp/mmrc-${runID}`;
  const tmuxSock = `magmux-${runID}`;
  const tmuxSession = `magmux-${runID}`;
  const demoEnv = {
    ...process.env,
    NO_BROWSER: "1",
    NO_ATTACH: "1",
    // OWN-SERVER MODE, PINNED. This check is about the private-server shape —
    // its own session, its `client-attached` hook, its `kill-server` teardown —
    // and demo.sh picks its mode by whether $TMUX is set. This suite is very
    // often run from inside tmux (it is a terminal multiplexer's repository),
    // and without this line the launcher would correctly adopt that tmux and
    // every assertion below about a session and a private server would be
    // measuring the other mode. Check 10 is where adoption is measured.
    RC_DEMO_TMUX: "own",
    RC_DEMO_ID: runID,
    RC_DEMO_STATE: runState,
    RC_DEMO_KEEP: "0",
    // THE THREE-PANE SHAPE, ASKED FOR BY NAME. The terminal mirror is off by
    // default now — magmux gets the rows instead, which is what check 14
    // measures — but this check is the one that proves a SECOND PROCESS paints
    // the same frames off the WebSocket, so it asks for the mirror pane. 40
    // rows holds all three comfortably (the shape needs 33).
    RC_DEMO_MIRROR: "1",
    // And the tmux-pane shape, pinned: RC_DEMO_GEOM would take magmux out of
    // tmux entirely and there would be no pane A to capture.
    RC_DEMO_GEOM: "off",
    // Fixed geometry. demo.sh would otherwise ask `tput` about a stdout that
    // is a pipe here and fall back to 120x40 anyway; saying it out loud is what
    // stops the captures below being measured at a different size per machine.
    RC_DEMO_COLS: "120",
    RC_DEMO_ROWS: "40",
  };

  /** One tmux command on the demo's PRIVATE server. Never the operator's. */
  const tmux = (...args: string[]): { code: number; out: string } => {
    const r = spawnSync("tmux", ["-L", tmuxSock, ...args], { encoding: "utf8" });
    return { code: r.status ?? -1, out: `${r.stdout ?? ""}${r.stderr ?? ""}` };
  };
  /** stop.sh, the documented escape hatch, carrying this run's identity. */
  const stopSh = (): { code: number; out: string } => {
    const r = spawnSync("bash", [path.join(HERE, "stop.sh")], { encoding: "utf8", env: demoEnv });
    return { code: r.status ?? -1, out: `${r.stdout ?? ""}${r.stderr ?? ""}` };
  };
  /**
   * Anything still alive carrying this run's unique id in its argv — magmux
   * (`--id`), serve.ts (`--state`), cli.ts (`--token-file`), the driver (`--id`)
   * and the ticket-runner plugin (`--demo-id`, an argument it ignores and which
   * exists so it is findable here) all do. By the id and never by
   * `pkill -f magmux`, which is how somebody loses the magmux they were working
   * in.
   */
  const survivors = (pattern: string): string =>
    (spawnSync("pgrep", ["-fl", pattern], { encoding: "utf8" }).stdout ?? "").trim();
  const plain = (s: string) => s.replace(/\x1b\[[0-9;]*m/g, "");

  let demo: ChildProcess | null = null;
  let demoOut = "";
  let demoExit: number | null = null;

  try {
    const launchedAt = Date.now();
    demo = spawn("bash", [path.join(HERE, "demo.sh")], {
      stdio: ["ignore", "pipe", "pipe"],
      env: demoEnv,
    });
    demo.stdout?.on("data", (b) => {
      demoOut += b.toString();
    });
    demo.stderr?.on("data", (b) => {
      demoOut += b.toString();
    });
    demo.on("exit", (code) => {
      demoExit = code;
    });

    // A poll on the banner with a deadline, never a sleep: exactly the idiom
    // demo.sh uses on its own two readiness waits. `demoExit !== null` ends the
    // wait early, so a launcher that DIED reports its own output instead of
    // spending the whole budget.
    await until(
      `demo.sh to print its NO_ATTACH banner (${tmuxV})`,
      LAUNCH_BUDGET_MS,
      () => /holding the session open/.test(demoOut) || demoExit !== null,
    );
    const bannerAt = Date.now();
    const banner = plain(demoOut);

    // `$STATE/magmux.url` is THE channel to the browser page — serve.ts reads
    // it per request and answers 503 until it appears — so it is read here the
    // way the page reads it, and then actually used.
    const urlPath = path.join(runState, "magmux.url");
    const demoURL = fs.existsSync(urlPath) ? fs.readFileSync(urlPath, "utf8").trim() : "";
    check(
      "demo.sh reaches its ready banner, naming magmux's URL and the browser page",
      demoExit === null &&
        /holding the session open/.test(banner) &&
        demoURL.startsWith("http") &&
        banner.includes(demoURL) &&
        /\/\?c=\/c\/[A-Za-z0-9_-]{32}\.json/.test(banner),
      demoExit !== null
        ? `demo.sh exited ${demoExit} before its banner: ${JSON.stringify(
            banner.trim().split("\n").slice(-3).join(" ⏎ ").slice(0, 160),
          )}`
        : `ready in ${bannerAt - launchedAt}ms; magmux.url=${demoURL || "(absent)"}, ` +
          `page URL in the banner=${/\/\?c=\/c\//.test(banner)}`,
    );

    // "A URL that answers" means answers AS THE DEMO USES IT: the read-only
    // view token demo.sh minted, against the port demo.sh wrote down.
    const smokeToken = fs.readFileSync(path.join(runState, "view.token"), "utf8").trim();
    const caps = await fetch(`${demoURL}/v1/capabilities`, {
      headers: { Authorization: `Bearer ${smokeToken}` },
    });
    const capsJSON: any = caps.status === 200 ? await caps.json() : null;
    check(
      "$STATE/magmux.url holds a URL that answers, to this run's READ-ONLY token",
      caps.status === 200 && capsJSON?.readOnly === true,
      `GET ${demoURL}/v1/capabilities → ${caps.status}, readOnly=${capsJSON?.readOnly}`,
    );

    // The two panes, by the PANE IDS the banner advertises — so what is
    // captured below is exactly what the banner told a human to capture.
    const paneA = /capture-pane -p -t (%\d+)\s+\(magmux\)/.exec(banner)?.[1] ?? "";
    const paneB = /capture-pane -p -t (%\d+)\s+\(the viewer\)/.exec(banner)?.[1] ?? "";
    const paneC = /capture-pane -p -t (%\d+)\s+\(the driver\)/.exec(banner)?.[1] ?? "";
    // THREE surfaces now, and the session is driven from the third. `-a` because
    // the driver is a pane of the same window at 40 rows and a window of its own
    // below that floor, and this check must pass either way — what it asserts is
    // that all three exist and are the three the banner told a human to capture,
    // not which window tmux put them in.
    const listed = tmux("list-panes", "-a", "-t", tmuxSession, "-F", "#{pane_id}")
      .out.trim()
      .split("\n")
      .filter(Boolean);
    check(
      "the tmux session has exactly three panes — magmux, the mirror and the driver",
      listed.length === 3 &&
        paneA !== "" &&
        paneB !== "" &&
        paneC !== "" &&
        listed.includes(paneA) &&
        listed.includes(paneB) &&
        listed.includes(paneC),
      `list-panes=[${listed.join(",")}]; the banner says magmux=${paneA || "?"}, viewer=${paneB || "?"}, driver=${
        paneC || "?"
      }`,
    );

    const capture = (p: string) => (p ? tmux("capture-pane", "-p", "-t", p).out : "");
    await until("pane A (magmux) to paint the colour card", PAINT_BUDGET_MS, () =>
      capture(paneA).includes(CARD_MARKER),
    ).catch(() => {});
    const capA = capture(paneA);
    check(
      "tmux capture of pane A shows magmux painting the colour card",
      capA.includes(CARD_MARKER),
      capA.includes(CARD_MARKER)
        ? `${JSON.stringify(CARD_MARKER)} found in ${capA.split("\n").length} captured rows`
        : `no ${JSON.stringify(CARD_MARKER)} in pane A: ${JSON.stringify(capA.trim().slice(0, 140))}`,
    );

    // THE ONE THAT MATTERS. Pane B is `cli.ts` — a separate process, a real
    // WebSocket, the shared decoder and the ANSI painter — so the same marker
    // appearing there is the whole demo working through the real launcher: one
    // hub, one framer, two surfaces. No other check in this file paints.
    await until("pane B (the CLI client) to mirror the card", PAINT_BUDGET_MS, () =>
      capture(paneB).includes(CARD_MARKER),
    ).catch(() => {});
    const capB = capture(paneB);
    check(
      "and pane B — the CLI client — shows the SAME marker: the mirror, end to end",
      capB.includes(CARD_MARKER),
      capB.includes(CARD_MARKER)
        ? `the viewer pane carries ${JSON.stringify(CARD_MARKER)} too, painted from frames off the WebSocket`
        : `no ${JSON.stringify(CARD_MARKER)} in pane B: ${JSON.stringify(capB.trim().slice(0, 140))}`,
    );

    // The DRIVER pane. It is the surface a human actually touches, it holds the
    // FULL session token where both mirrors hold the read-only one, and nothing
    // else in this file executes it. A driver that failed to read its token, or
    // that magmux refused, dies before its menu and the pane shows the error
    // instead — so asserting on a menu line is asserting that the credential
    // resolved, the URL answered and `capabilities` came back readOnly=false.
    // WITH SCROLLBACK, unlike the two panes above. The driver's pane is ~25% of
    // the window — 10 rows at this geometry — and its header and menu are some
    // 25 lines, so the visible screen is the tail of them. The question here is
    // what the driver PRINTED, which is the whole of its output; that it is
    // still waiting for a choice is asserted separately, on the live screen.
    const captureAll = (p: string) => (p ? tmux("capture-pane", "-p", "-S", "-400", "-t", p).out : "");
    await until("pane C (the driver) to print its menu", PAINT_BUDGET_MS, () =>
      captureAll(paneC).includes(DRIVER_MARKER),
    ).catch(() => {});
    const capC = captureAll(paneC);
    const named = capC.includes("FULL SESSION TOKEN") && capC.includes("READ-ONLY VIEW TOKEN");
    check(
      "pane C is the driver, and it printed its menu naming BOTH credentials",
      capC.includes(DRIVER_MARKER) && named,
      capC.includes(DRIVER_MARKER)
        ? `${JSON.stringify(DRIVER_MARKER)} in ${capC.split("\n").length} rows of scrollback; both credentials named: ${named}`
        : `no ${JSON.stringify(DRIVER_MARKER)} in pane C: ${JSON.stringify(capC.trim().slice(-200))}`,
    );
    // …and it is WAITING, not finished. A driver that read its token, drew its
    // first frame and then fell over would satisfy everything above; the key
    // bar on the LIVE screen is what says it is still there to press a key in.
    // `capabilities ok · … readOnly=false` is on the same frame and is the
    // proof the session token was accepted, which is the credential the two
    // mirrors deliberately do not have — the TUI puts it in the response
    // panel's opening card rather than only in its log, because the demo's own
    // driver pane is fourteen rows and has no log panel at that size.
    const liveC = capture(paneC);
    const keybar = /enter\s+run/.test(liveC) && /tab\s+focus/.test(liveC);
    check(
      "the driver is still waiting with its key bar up, and the session token accepted",
      keybar && capC.includes("capabilities ok") && capC.includes("readOnly=false"),
      `key bar on screen=${keybar}; ` +
        `${JSON.stringify((/capabilities ok[^\n]*/.exec(capC)?.[0] ?? "(no capabilities line)").slice(0, 110))}`,
    );

    // THE NON-INTERACTIVE HALF, against the same running demo. A TUI cannot be
    // driven by piping stdin, so `--run N` is the driver's other face: one
    // action, the same evidence as plain text, and an exit code that means
    // something. Action 8 is the one worth spending a check on — it is the
    // credential boundary, and it must come back as magmux's own 403 AND as a
    // SUCCESS, because the refusal is the thing being demonstrated.
    const run8 = spawnSync(driverBin(), ["--state", runState, "--id", runID, "--run", "8"], {
      encoding: "utf8",
      env: { ...process.env, NO_COLOR: "1" },
    });
    const run8Out = noAnsi(`${run8.stdout ?? ""}${run8.stderr ?? ""}`);
    check(
      "`--run 8` prints magmux's own 403 and exits 0 — a refusal that was the point is a PASS",
      run8.status === 0 &&
        /HTTP 403/.test(run8Out) &&
        /"code":"forbidden"/.test(run8Out) &&
        /BOUNDARY PROVEN/.test(run8Out) &&
        /as the VIEW token/.test(run8Out),
      `exit=${run8.status}: ${JSON.stringify(
        run8Out.trim().split("\n").filter(Boolean).slice(-3).join(" ⏎ ").slice(0, 220),
      )}`,
    );
    // …and the same request with the FULL token is a 200, so the 403 above is
    // about the CREDENTIAL and not about the op, the pane or the demo being
    // broken. Two runs of one binary, one difference between them.
    const run1 = spawnSync(driverBin(), ["--state", runState, "--id", runID, "--run", "1"], {
      encoding: "utf8",
      env: { ...process.env, NO_COLOR: "1" },
    });
    const run1Out = noAnsi(`${run1.stdout ?? ""}${run1.stderr ?? ""}`);
    check(
      "…and `--run 1`, the same POST with the SESSION token, is a 200: the 403 is about the credential",
      run1.status === 0 && /HTTP 200/.test(run1Out) && /as the session token/.test(run1Out),
      `exit=${run1.status}: ${JSON.stringify(
        run1Out.trim().split("\n").filter(Boolean).slice(-2).join(" ⏎ ").slice(0, 180),
      )}`,
    );

    // The plugin the driver's item 10 needs. `--plugin` is in server.sh's flag
    // set, and a plugin that fails to start is skipped rather than fatal — so
    // without this check the demo would come up looking perfect with item 10
    // quietly unavailable. Asked through the op table, which is what item 10
    // itself checks.
    const opsRes = await fetch(`${demoURL}/v1/ops`, {
      headers: { Authorization: `Bearer ${fs.readFileSync(path.join(runState, `magmux-${runID}.token`), "utf8").trim()}` },
    });
    const opsBody: any = opsRes.status === 200 ? await opsRes.json() : null;
    const opNames: string[] = (opsBody?.ops ?? []).map((o: any) => o.name);
    check(
      "the ticket-runner plugin registered, so the driver's item 10 has a real op to call",
      opNames.includes("ticket.run_ticket"),
      opNames.includes("ticket.run_ticket")
        ? `op rev ${opsBody?.rev}; the plugin added ${opNames.filter((n) => n.includes(".")).join(", ")}`
        : `no ticket.run_ticket among ${opNames.length} ops: ${opNames.join(", ").slice(0, 120)}`,
    );

    // The destroy-unattached guard. Nothing has attached, and nothing will.
    const waitMore = UNATTACHED_PROOF_MS - (Date.now() - bannerAt);
    if (waitMore > 0) await sleep(waitMore);
    const aliveMs = Date.now() - bannerAt;
    const alive = tmux("has-session", "-t", tmuxSession).code === 0;
    check(
      `the session is still alive ${Math.round(aliveMs / 1000)}s after creation with NOTHING attached`,
      alive,
      alive
        ? `has-session ok after ${aliveMs}ms unattached — the measured destroy-unattached failure took the whole server in ~3000ms`
        : `the session is GONE after ${aliveMs}ms unattached: destroy-unattached is being set before a client attaches — ${JSON.stringify(
            tmux("list-sessions").out.trim().slice(0, 100),
          )}`,
    );

    // ── teardown: criterion 7 and R8, which nothing has ever executed ──────
    const stop1 = stopSh();
    // demo.sh's NO_ATTACH loop polls has-session, so killing the session is
    // what releases ITS trap: the launcher, not stop.sh, is what removes
    // $STATE here. Waiting for that exit is what makes the assertions below
    // about the launcher's teardown rather than about stop.sh's.
    await until("demo.sh to exit after its session was killed", 30_000, () => demoExit !== null);
    const stop2 = stopSh();
    check(
      "stop.sh exits 0, and a second stop.sh is an idempotent no-op that also exits 0",
      stop1.code === 0 && stop2.code === 0 && /nothing was running/.test(plain(stop2.out)),
      `first=${stop1.code}, second=${stop2.code}; the second said ${JSON.stringify(
        plain(stop2.out).trim().split("\n").pop()?.slice(0, 70) ?? "",
      )}`,
    );

    // Matched on a LISTING LINE (`<session>: 1 windows …`), not on a substring
    // of the whole output: tmux's own refusal names the socket, and this demo's
    // socket and session deliberately share a name, so a substring test can
    // never pass. The refusal itself is the assertion — no server, not merely
    // no session, because the private server is this demo's to take with it.
    const ls = tmux("list-sessions");
    const stillListed = new RegExp(`^${tmuxSession}:`, "m").test(ls.out);
    check(
      "after stop there is no tmux session and no private tmux server left",
      ls.code !== 0 && !stillListed && /no server running/.test(ls.out),
      `list-sessions → exit ${ls.code}: ${JSON.stringify(ls.out.trim().slice(0, 90))}`,
    );

    const left = survivors(runID);
    check(
      "no magmux, serve.ts, mirror, driver or plugin survives the run, and $STATE is gone",
      left === "" && !fs.existsSync(runState) && demoExit === 0,
      left === ""
        ? `nothing matches ${runID}; ${runState} removed; demo.sh exited ${demoExit}`
        : `still running: ${JSON.stringify(left.split("\n").join(" ⏎ ").slice(0, 160))}`,
    );
  } catch (err: any) {
    check("the launcher smoke check ran to completion", false, String(err?.message ?? err));
    const tail = plain(demoOut).trim().split("\n").slice(-25);
    if (tail.length) {
      console.log(`\n  ${C.bold}demo.sh output (tail)${C.reset}`);
      for (const l of tail) console.log(`  ${C.grey}│${C.reset} ${l.slice(0, 160)}`);
    }
  } finally {
    // EVERY path, an exception included. A check that leaks a tmux server and
    // a listening port holding a live feed of somebody's shell is worse than
    // no check at all — which is the rule demo.sh's own teardown is built on.
    demo?.kill("SIGTERM");
    stopSh();
    fs.rmSync(runState, { recursive: true, force: true });
    // And the socket INODE, which is this check's own litter rather than the
    // demo's: a per-run id means a per-run name under /tmp/tmux-$UID, and tmux
    // leaves the file behind when the server goes. Unlinked only once tmux
    // itself says there is no server on it, and only at a name this run
    // invented, so it can never reach a session somebody is working in. The
    // path comes from tmux's own refusal rather than from a rebuilt guess at
    // TMUX_TMPDIR.
    const dead = /no server running on (\S+)/.exec(tmux("list-sessions").out);
    if (dead) fs.rmSync(dead[1], { force: true });
  }
}

// ── check 10: ADOPT MODE — the demo as a guest in a tmux that already exists ─
//
// Check 9 above runs the launcher OUTSIDE tmux, which is own-server mode: a
// private server, its own session, its own windows, and a teardown that may
// `kill-server` because nothing of anybody's is on it. That is still the right
// behaviour there and it is still checked.
//
// This is the other half, and the one with teeth. With $TMUX set the launcher
// adopts the tmux it is already in: it splits THE PANE IT IS RUNNING IN, puts
// magmux and the mirror above itself, and runs the driver in the foreground
// right where the operator typed. The failure this guards against is not
// cosmetic — a demo that reached for `kill-server`, `kill-session` or
// `destroy-unattached` in that mode would take somebody's work with it when it
// exited, and nothing else in this repository would notice.
//
// So a THROWAWAY server stands in for the operator's. It is given two windows
// of its own before the demo starts, and after teardown both of them, their
// pane counts and the server itself must be exactly as they were.
//
// HOW THE DRIVER IS DRIVEN: demo.sh is spawned with a PIPE on stdin and the
// driver now runs in the launcher's own process group, in the foreground — so
// the pipe IS the driver's keyboard, and writing "q\n" into it is the same
// keypress an operator would make. That is preferred over killing the launcher:
// `q` is the documented way to end an adopted run, and it exercises the whole
// path — driver returns, launcher's trap fires, panes go — rather than only the
// signal handler.
console.log(`\n${C.bold}  10 · adopt mode: a guest in an existing tmux, and the bystanders survive${C.reset}`);

if (!tmuxV) {
  console.log(`  ${C.yellow}SKIPPED${C.reset} ${C.grey}— tmux is not installed.${C.reset}`);
} else {
  const runID = `adopt-${process.pid}-${crypto.randomBytes(3).toString("hex")}`;
  const runState = `/tmp/mmrc-${runID}`;
  const hostSock = `magmux-host-${runID}`;
  const hostSession = "operator";
  const plain = (s: string) => s.replace(/\x1b\[[0-9;]*m/g, "");

  /** One command on the THROWAWAY server standing in for the operator's. */
  const htmux = (...args: string[]): { code: number; out: string } => {
    const r = spawnSync("tmux", ["-L", hostSock, ...args], { encoding: "utf8" });
    return { code: r.status ?? -1, out: `${r.stdout ?? ""}${r.stderr ?? ""}` };
  };
  const windowIDs = () =>
    htmux("list-windows", "-a", "-F", "#{window_id} #{window_name}").out.trim().split("\n").filter(Boolean);
  const panesIn = (target: string) =>
    htmux("list-panes", "-t", target, "-F", "#{pane_id} #{pane_height} #{pane_active} #{@rc_demo}")
      .out.trim()
      .split("\n")
      .filter(Boolean);

  let demo: ChildProcess | null = null;
  let demoOut = "";
  let demoExit: number | null = null;

  try {
    // The operator's session: one bystander window they are working in, and one
    // "workbench" window whose single pane stands in for the pane they typed
    // `task demo:rc` into. 50 rows, so the three-pane layout is comfortable.
    htmux("new-session", "-d", "-s", hostSession, "-n", "bystander", "-x", "200", "-y", "50", "sleep 600");
    htmux("new-window", "-t", hostSession, "-n", "workbench", "-P", "-F", "#{pane_id}", "sleep 600");
    const launcherPane = htmux("display-message", "-p", "-t", `${hostSession}:workbench`, "#{pane_id}").out.trim();
    const socketPath = htmux("display-message", "-p", "#{socket_path}").out.trim();
    const serverPID = htmux("display-message", "-p", "#{pid}").out.trim();
    const windowsBefore = windowIDs();

    // $TMUX is exactly what tmux sets in a pane: socket path, server pid,
    // session index. $TMUX_PANE names the pane — which is how demo.sh knows
    // which one to split, rather than splitting whatever the client happens to
    // be looking at.
    const adoptEnv = {
      ...process.env,
      TMUX: `${socketPath},${serverPID},0`,
      TMUX_PANE: launcherPane,
      NO_BROWSER: "1",
      RC_DEMO_ID: runID,
      RC_DEMO_STATE: runState,
      RC_DEMO_KEEP: "0",
      // The three-pane shape by name, for the same reason check 9 asks for it:
      // this check's subject is the two panes the demo CREATES in somebody
      // else's window, and the mirror is the second of them. The default shape
      // — magmux and the driver only — is check 14's subject.
      RC_DEMO_MIRROR: "1",
      RC_DEMO_GEOM: "off",
    };
    delete (adoptEnv as any).NO_ATTACH; // adopt mode never attaches anything

    demo = spawn("bash", [path.join(HERE, "demo.sh")], {
      stdio: ["pipe", "pipe", "pipe"],
      env: adoptEnv,
    });
    demo.stdout?.on("data", (b) => {
      demoOut += b.toString();
    });
    demo.stderr?.on("data", (b) => {
      demoOut += b.toString();
    });
    demo.on("exit", (code) => {
      demoExit = code;
    });

    await until(
      "demo.sh to report the adopted demo ready",
      LAUNCH_BUDGET_MS,
      () => /ready — the demo is running in this window/.test(plain(demoOut)) || demoExit !== null,
    );

    // NO NEW WINDOW. The whole design in one assertion: the demo lives in panes
    // of the window that was already there.
    const windowsDuring = windowIDs();
    check(
      "adopt mode creates NO new tmux window — the demo is panes of the one that was there",
      demoExit === null && windowsDuring.length === windowsBefore.length && windowsDuring.join("|") === windowsBefore.join("|"),
      demoExit !== null
        ? `demo.sh exited ${demoExit} before it was ready: ${JSON.stringify(
            plain(demoOut).trim().split("\n").slice(-3).join(" ⏎ ").slice(0, 200),
          )}`
        : `windows before=[${windowsBefore.join(", ")}] during=[${windowsDuring.join(", ")}]`,
    );

    // …and the launcher's own pane was split, twice, with the operator's pane
    // still the active one: a guest does not take the keyboard.
    const during = panesIn(`${hostSession}:workbench`);
    const ours = during.filter((l) => l.endsWith(` ${runID}`));
    const theirs = during.filter((l) => l.startsWith(`${launcherPane} `));
    check(
      "it split the launcher's OWN pane into three, stamped its two, and left focus alone",
      during.length === 3 && ours.length === 2 && theirs.length === 1 && theirs[0]!.split(" ")[2] === "1",
      `panes now: ${JSON.stringify(during)} (ours are marked @rc_demo=${runID})`,
    );

    // The same marker check 9 uses, on the two panes the demo created: magmux
    // painting the colour card, and a separate process mirroring it off the
    // WebSocket. Top to bottom, so pane one is magmux and pane two is the
    // mirror; the launcher's own pane is last and is the driver.
    const ids = during.map((l) => l.split(" ")[0]!);
    const capture = (p: string) => htmux("capture-pane", "-p", "-t", p).out;
    await until("the adopted magmux pane to paint the colour card", PAINT_BUDGET_MS, () =>
      capture(ids[0]!).includes(CARD_MARKER),
    ).catch(() => {});
    await until("the adopted mirror pane to mirror it", PAINT_BUDGET_MS, () =>
      capture(ids[1]!).includes(CARD_MARKER),
    ).catch(() => {});
    const capA = capture(ids[0]!);
    const capB = capture(ids[1]!);
    check(
      "the pane above is magmux painting the card, and the one below it is the MIRROR",
      capA.includes(CARD_MARKER) && capB.includes(CARD_MARKER),
      capA.includes(CARD_MARKER) && capB.includes(CARD_MARKER)
        ? `${JSON.stringify(CARD_MARKER)} on both ${ids[0]} (magmux) and ${ids[1]} (the mirror), off the WebSocket`
        : `magmux pane has it: ${capA.includes(CARD_MARKER)}; mirror pane has it: ${capB.includes(
            CARD_MARKER,
          )} — ${JSON.stringify(capB.trim().slice(0, 140))}`,
    );

    // The driver is this launcher's own foreground process, so its menu is on
    // demo.sh's stdout rather than in a pane. Same two claims as check 9: it
    // printed its menu, and magmux accepted the session token.
    await until("the driver to print its menu on the launcher's own stdout", PAINT_BUDGET_MS, () =>
      /choose.*1-13/.test(plain(demoOut)),
    ).catch(() => {});
    const driverOut = plain(demoOut);
    check(
      "the driver runs in the operator's own pane, with the session token accepted",
      driverOut.includes(DRIVER_MARKER) &&
        driverOut.includes("FULL SESSION TOKEN") &&
        driverOut.includes("readOnly=false") &&
        /choose.*1-13/.test(driverOut),
      `menu on the launcher's stdout=${driverOut.includes(DRIVER_MARKER)}; ` +
        `${JSON.stringify((/capabilities ok[^\n]*/.exec(driverOut)?.[0] ?? "(no capabilities line)").slice(0, 100))}`,
    );

    // ── teardown, by the documented keystroke ───────────────────────────────
    demo.stdin?.write("q\n");
    await until("the driver to quit and the launcher to tear the demo down", 60_000, () => demoExit !== null);

    const after = panesIn(`${hostSession}:workbench`);
    const windowsAfter = windowIDs();
    const serverAlive = htmux("has-session", "-t", hostSession).code === 0;
    check(
      "after `q`: the operator's pane is alone again, at full height, and their window count is unchanged",
      after.length === 1 && after[0]!.startsWith(`${launcherPane} `) && after[0]!.split(" ")[1] === "50",
      `panes in that window: ${JSON.stringify(after)} (the launcher's pane was ${launcherPane})`,
    );
    check(
      "THE BYSTANDER SURVIVES: both of the operator's windows are still there, and so is their server",
      serverAlive &&
        windowsAfter.join("|") === windowsBefore.join("|") &&
        windowsAfter.some((w) => w.endsWith(" bystander")),
      serverAlive
        ? `windows after=[${windowsAfter.join(", ")}] — identical to before, server pid ${serverPID} still running`
        : `THE SERVER IS GONE: adopt mode reached for kill-server or kill-session — ${JSON.stringify(
            htmux("list-sessions").out.trim().slice(0, 120),
          )}`,
    );
    // POLLED, not sampled. `kill-pane` is a signal, and magmux and the mirror
    // get the few milliseconds every process gets to die in; asserting on the
    // instant demo.sh returned would make this check a race that fails on a
    // loaded machine and passes everywhere else.
    // The run's unique id is in the argv of everything the demo starts —
    // magmux (`--id`), serve.ts (`--state`), cli.ts (`--token-file`), the driver
    // and the plugin (`--demo-id`). It is ALSO in the argv of the throwaway
    // tmux server this check started to stand in for the operator's, which is
    // this file's own scaffolding and is torn down below; `magmux-host-` is the
    // name only that server has, so dropping those lines asks the question that
    // was meant — did the DEMO leave anything behind.
    const left = () =>
      (spawnSync("pgrep", ["-fl", runID], { encoding: "utf8" }).stdout ?? "")
        .split("\n")
        .filter((l) => l.trim() !== "" && !l.includes(`magmux-host-`))
        .join("\n")
        .trim();
    await until("every process of the adopted run to be gone", 15_000, () => left() === "").catch(() => {});
    const survivors = left();
    check(
      "the adopted run leaves no process and no $STATE, and demo.sh exits 0",
      survivors === "" && !fs.existsSync(runState) && demoExit === 0,
      survivors === ""
        ? `demo.sh exited ${demoExit}; ${runState} removed; nothing matches ${runID}`
        : `still running: ${JSON.stringify(survivors.split("\n").join(" ⏎ ").slice(0, 200))}`,
    );
  } catch (err: any) {
    check("the adopt-mode check ran to completion", false, String(err?.message ?? err));
    const tail = plain(demoOut).trim().split("\n").slice(-20);
    if (tail.length) {
      console.log(`\n  ${C.bold}demo.sh output (tail, adopt mode)${C.reset}`);
      for (const l of tail) console.log(`  ${C.grey}│${C.reset} ${l.slice(0, 160)}`);
    }
  } finally {
    // EVERY path. The throwaway server is this check's own litter: killing its
    // only session takes the server with it, which is deliberately NOT what the
    // demo is allowed to do and is exactly what a test that created the server
    // must do.
    demo?.kill("SIGTERM");
    spawnSync("bash", [path.join(HERE, "stop.sh")], {
      encoding: "utf8",
      env: { ...process.env, RC_DEMO_ID: runID, RC_DEMO_STATE: runState },
    });
    htmux("kill-session", "-t", hostSession);
    fs.rmSync(runState, { recursive: true, force: true });
    const dead = /no server running on (\S+)/.exec(htmux("list-sessions").out);
    if (dead) fs.rmSync(dead[1], { force: true });
  }
}

// ── checks 11-13: the HEIGHT BUDGET, and what a dead magmux looks like ──────
//
// The defect these exist for passed all 57 checks above. On a 24-row window the
// launcher warned that the three-pane layout wanted 33 rows, said "Building
// panes anyway", and built magmux a 2-row pane. magmux binds its listener and
// writes its token BEFORE it lays panes out, then refuses a pane under
// `minPaneRows` and exits — removing the token on the way, as it is documented
// to. What the operator saw, a second later and three steps downstream, was the
// driver's "cannot read the session token … is the demo running?", and the time
// went on tokens, auth and file modes, all of which were fine.
//
// So: a launcher that builds a layout magmux would refuse is broken (magmux's
// own rule is "CREATION refuses; RESHAPE clamps" — the demo must not do the
// thing the product refuses to do), and a failure reported three steps from its
// cause is broken twice. Both halves are checked here, and the third check is
// the one that stops the DIAGNOSIS regressing silently: it kills magmux after
// it has bound, and asserts magmux's own words reach the operator.
//
// The geometry is PINNED with RC_DEMO_ROWS in all three, so "30 rows" means 30
// rows here rather than "whatever tmux made of -y 30 after its status line".
// The first two run on a private server, which is where the decision is cheapest
// to observe; the third runs ADOPTED, because that is the only mode where a pane
// is CREATED at the size the arithmetic chose — see its own note.
console.log(`\n${C.bold}  11 · a short window: the launcher DEGRADES to two panes, and the demo works${C.reset}`);

/** RC_DEMO_* knobs that would make these three checks measure the operator's shell. */
const cleanDemoEnv = () => {
  const e: Record<string, string> = { ...(process.env as Record<string, string>) };
  for (const k of [
    "RC_DEMO_MIRROR",
    "RC_DEMO_LAYOUT",
    "RC_DEMO_MIN_ROWS",
    "RC_DEMO_DRIVER_ROWS",
    "RC_DEMO_DRIVER",
    "RC_DEMO_TMUX",
    "RC_DEMO_COLS",
    "RC_DEMO_ROWS",
    "RC_DEMO_GEOM",
  ])
    delete e[k];
  return e;
};

/** Everything still alive carrying a run's unique id in its argv. */
const aliveWith = (pattern: string): string =>
  (spawnSync("pgrep", ["-fl", pattern], { encoding: "utf8" }).stdout ?? "").trim();

/**
 * Run demo.sh to completion or to a predicate, collecting everything an
 * operator would see. One helper for all three checks below, because what each
 * of them asserts on is exactly that: the launcher's own output.
 */
async function runLauncher(opts: {
  env: Record<string, string>;
  until: (out: string, exited: number | null) => boolean;
  budget: number;
  what: string;
}): Promise<{ out: string; exit: number | null; proc: ChildProcess }> {
  const proc = spawn("bash", [path.join(HERE, "demo.sh")], {
    stdio: ["pipe", "pipe", "pipe"],
    env: opts.env,
  });
  let out = "";
  let exit: number | null = null;
  proc.stdout?.on("data", (b) => {
    out += b.toString();
  });
  proc.stderr?.on("data", (b) => {
    out += b.toString();
  });
  proc.on("exit", (code) => {
    exit = code;
  });
  await until(opts.what, opts.budget, () => opts.until(noAnsi(out), exit)).catch(() => {});
  return { out: noAnsi(out), exit, proc };
}

if (!tmuxV) {
  console.log(`  ${C.yellow}SKIPPED${C.reset} ${C.grey}— tmux is not installed.${C.reset}`);
} else {
  const runID = `short-${process.pid}-${crypto.randomBytes(3).toString("hex")}`;
  const runState = `/tmp/mmrc-${runID}`;
  const tmuxSock = `magmux-${runID}`;
  const tmuxSession = `magmux-${runID}`;
  const tmux = (...args: string[]) => {
    const r = spawnSync("tmux", ["-L", tmuxSock, ...args], { encoding: "utf8" });
    return { code: r.status ?? -1, out: `${r.stdout ?? ""}${r.stderr ?? ""}` };
  };
  let demo: ChildProcess | null = null;
  try {
    // 30 rows: under the 33 three panes need, over the 23 two panes need. The
    // demo must come up WORKING here — this is the common shape of a laptop
    // window, and it was the shape that produced a 2-row magmux pane.
    //
    // RC_DEMO_MIRROR=1 is what makes this a degrade to observe at all now that
    // the mirror is opt-in: it ASKS for the third pane, the window cannot hold
    // it, and the launcher says so and drops it. (A shape named by NAME —
    // RC_DEMO_LAYOUT=panes — is still built regardless; that is check 13.)
    const env = {
      ...cleanDemoEnv(),
      NO_BROWSER: "1",
      NO_ATTACH: "1",
      RC_DEMO_MIRROR: "1",
      RC_DEMO_TMUX: "own",
      RC_DEMO_ID: runID,
      RC_DEMO_STATE: runState,
      RC_DEMO_KEEP: "0",
      RC_DEMO_COLS: "120",
      RC_DEMO_ROWS: "30",
    };
    const r = await runLauncher({
      env,
      budget: LAUNCH_BUDGET_MS,
      what: "demo.sh to build the degraded two-pane demo",
      until: (out, exited) => /holding the session open/.test(out) || exited !== null,
    });
    demo = r.proc;

    const panes = tmux("list-panes", "-a", "-t", tmuxSession, "-F", "#{pane_id} #{pane_height}")
      .out.trim()
      .split("\n")
      .filter(Boolean);
    check(
      "30 rows: the launcher drops the MIRROR and builds TWO panes, saying so as a decision",
      r.exit === null &&
        /three panes want 33/.test(r.out) &&
        /builds TWO/.test(r.out) &&
        /drops the terminal mirror/.test(r.out) &&
        panes.length === 2,
      r.exit !== null
        ? `demo.sh exited ${r.exit}: ${JSON.stringify(r.out.trim().split("\n").slice(-4).join(" ⏎ ").slice(0, 220))}`
        : `panes=[${panes.join(", ")}]; the note was ${JSON.stringify(
            (/demo:rc: 30 rows[^\n]*/.exec(r.out)?.[0] ?? "(absent)").slice(0, 120),
          )}`,
    );

    // …AND IT WORKS. A degrade that produced a tidy message and a broken demo
    // would satisfy the check above, so this one asks for the whole thing:
    // magmux alive with its token on disk, painting the card, and a driver that
    // magmux accepted the full session token from.
    const paneA = /capture-pane -p -t (%\d+)\s+\(magmux\)/.exec(r.out)?.[1] ?? "";
    const paneC = /capture-pane -p -t (%\d+)\s+\(the driver\)/.exec(r.out)?.[1] ?? "";
    const cap = (p: string) => (p ? tmux("capture-pane", "-p", "-S", "-400", "-t", p).out : "");
    await until("the degraded magmux pane to paint the colour card", PAINT_BUDGET_MS, () =>
      cap(paneA).includes(CARD_MARKER),
    ).catch(() => {});
    await until("the driver to print its menu", PAINT_BUDGET_MS, () => cap(paneC).includes(DRIVER_MARKER)).catch(
      () => {},
    );
    const capA = cap(paneA);
    const capC = cap(paneC);
    const tokenOnDisk = fs.existsSync(path.join(runState, `magmux-${runID}.token`));
    check(
      "and the two-pane demo WORKS: magmux alive with its token, painting, and a driver it accepted",
      capA.includes(CARD_MARKER) &&
        capC.includes(DRIVER_MARKER) &&
        capC.includes("readOnly=false") &&
        tokenOnDisk &&
        !/magmux exited/.test(r.out) &&
        !/cannot read the session token/.test(r.out + capC),
      `card in the magmux pane=${capA.includes(CARD_MARKER)}; driver menu=${capC.includes(
        DRIVER_MARKER,
      )}; session token on disk=${tokenOnDisk}; ` +
        `${JSON.stringify((/capabilities ok[^\n]*/.exec(capC)?.[0] ?? "(no capabilities line)").slice(0, 100))}`,
    );

    // The mirror is GONE, not broken: the driver's own hints must say so, or the
    // demo spends its life telling a human to look at a pane that is not there.
    check(
      "the driver's hints point at the BROWSER, never at a mirror pane that is not there",
      /browser tab/.test(capC) && !/mirror pane above/.test(capC),
      `the driver said ${JSON.stringify(
        (/[^\n]*(browser tab|mirror)[^\n]*/i.exec(capC)?.[0] ?? "(nothing about where to look)").trim().slice(0, 120),
      )}`,
    );

    demo.kill("SIGTERM");
    await until("the degraded run to tear itself down", 30_000, () => !fs.existsSync(runState)).catch(() => {});
  } catch (err: any) {
    check("the short-window check ran to completion", false, String(err?.message ?? err));
  } finally {
    demo?.kill("SIGTERM");
    spawnSync("bash", [path.join(HERE, "stop.sh")], {
      encoding: "utf8",
      env: { ...process.env, RC_DEMO_ID: runID, RC_DEMO_STATE: runState },
    });
    tmux("kill-session", "-t", tmuxSession);
    fs.rmSync(runState, { recursive: true, force: true });
    const dead = /no server running on (\S+)/.exec(tmux("list-sessions").out);
    if (dead) fs.rmSync(dead[1], { force: true });
  }
}

// ── check 12: too short for even two panes — refuse BEFORE starting anything ─
console.log(`\n${C.bold}  12 · too short for two panes: refused up front, with nothing started${C.reset}`);
{
  const runID = `tiny-${process.pid}-${crypto.randomBytes(3).toString("hex")}`;
  const runState = `/tmp/mmrc-${runID}`;
  const tmuxSock = `magmux-${runID}`;
  try {
    // 16 rows cannot hold the driver's 14-row menu, a divider and a magmux pane
    // worth looking at. Refusing is cheap HERE and only here: after the static
    // server there is a port and a minted view token to take back, and after
    // magmux there is a session somebody may already be watching.
    const r = await runLauncher({
      env: {
        ...cleanDemoEnv(),
        NO_BROWSER: "1",
        NO_ATTACH: "1",
        RC_DEMO_TMUX: "own",
        RC_DEMO_ID: runID,
        RC_DEMO_STATE: runState,
        RC_DEMO_COLS: "120",
        RC_DEMO_ROWS: "16",
      },
      budget: 30_000,
      what: "demo.sh to refuse a 16-row window",
      until: (_out, exited) => exited !== null,
    });
    check(
      "16 rows is REFUSED, and the refusal names the rows it has, the rows it needs and a fix",
      r.exit === 1 &&
        /16 rows/.test(r.out) &&
        /needs 23/.test(r.out) &&
        /33 rows/.test(r.out) &&
        /RC_DEMO_DRIVER_ROWS/.test(r.out),
      `exit=${r.exit}: ${JSON.stringify(r.out.trim().split("\n").slice(0, 3).join(" ⏎ ").slice(0, 220))}`,
    );
    // NOTHING STARTED. The refusal is above the state directory, the static
    // server and magmux, so there is no port, no token, no tmux server and no
    // directory — and therefore nothing that could be left behind.
    const server = spawnSync("tmux", ["-L", tmuxSock, "list-sessions"], { encoding: "utf8" });
    const survivors = aliveWith(runID);
    check(
      "…before the state directory, the static server, magmux or a tmux server existed",
      !fs.existsSync(runState) &&
        survivors === "" &&
        (server.status ?? -1) !== 0 &&
        /Nothing has been started/.test(r.out) &&
        !/token/i.test(r.out.split("Fixes:")[0] ?? ""),
      `$STATE exists=${fs.existsSync(runState)}; tmux -L ${tmuxSock} → ${JSON.stringify(
        `${server.stdout ?? ""}${server.stderr ?? ""}`.trim().slice(0, 60),
      )}; survivors=${JSON.stringify(survivors.slice(0, 80))}`,
    );
  } catch (err: any) {
    check("the too-short check ran to completion", false, String(err?.message ?? err));
  } finally {
    fs.rmSync(runState, { recursive: true, force: true });
  }
}

// ── check 13: THE MISATTRIBUTION GUARD ──────────────────────────────────────
//
// magmux dies AFTER it has bound and written its token, which is the one
// failure shape that reads as somebody else's fault. Nothing else in this file
// would notice the diagnosis regressing: the demo simply fails, as it should,
// with a message about the wrong thing — which is how this cost an afternoon.
//
// The way magmux is made to die is the original reproduction rather than a
// contrived kill: a three-pane layout is FORCED (`RC_DEMO_LAYOUT=panes`) on a
// window too short for it, so magmux gets a 3-row pane, refuses to lay out one
// pane in 80x2 and exits. That also checks the override's own promise — it
// warns that magmux may refuse — so the escape hatch cannot quietly become the
// silent-failure path again.
console.log(`\n${C.bold}  13 · magmux dies after binding: the operator gets MAGMUX'S reason, not a missing token${C.reset}`);

if (!tmuxV) {
  console.log(`  ${C.yellow}SKIPPED${C.reset} ${C.grey}— tmux is not installed.${C.reset}`);
} else {
  const runID = `dead-${process.pid}-${crypto.randomBytes(3).toString("hex")}`;
  const runState = `/tmp/mmrc-${runID}`;
  const hostSock = `magmux-host-${runID}`;
  const hostSession = "operator";
  const htmux = (...args: string[]) => {
    const r = spawnSync("tmux", ["-L", hostSock, ...args], { encoding: "utf8" });
    return { code: r.status ?? -1, out: `${r.stdout ?? ""}${r.stderr ?? ""}` };
  };
  let demo: ChildProcess | null = null;
  try {
    // ADOPT MODE, because that is where a pane is CREATED at the size the
    // arithmetic chose. In own-server mode magmux is started in a full-height
    // window and only shrinks when the mirror splits it — a RESHAPE, which
    // magmux clamps rather than refusing, so magmux survives a squeeze it would
    // have refused at birth. That asymmetry is the product's own rule
    // ("CREATION refuses; RESHAPE clamps") and it is why the original
    // reproduction was an adopted tmux: the split gives magmux its 3 rows up
    // front, and magmux refuses to lay out one pane in 80x2.
    htmux("new-session", "-d", "-s", hostSession, "-n", "workbench", "-x", "80", "-y", "50", "sleep 600");
    const launcherPane = htmux("display-message", "-p", "-t", `${hostSession}:workbench`, "#{pane_id}").out.trim();
    const socketPath = htmux("display-message", "-p", "#{socket_path}").out.trim();
    const serverPID = htmux("display-message", "-p", "#{pid}").out.trim();

    const r = await runLauncher({
      env: {
        ...cleanDemoEnv(),
        TMUX: `${socketPath},${serverPID},0`,
        TMUX_PANE: launcherPane,
        NO_BROWSER: "1",
        RC_DEMO_ID: runID,
        RC_DEMO_STATE: runState,
        RC_DEMO_KEEP: "0",
        RC_DEMO_LAYOUT: "panes", // asked for BY NAME: build it anyway
        // …and the third pane it needs in order to be too short. Named
        // together, these are the shape the launcher is not allowed to
        // substitute — which is the promise this check also tests.
        RC_DEMO_MIRROR: "1",
        RC_DEMO_COLS: "80",
        RC_DEMO_ROWS: "24", // the reproduction's window, to the row
      },
      budget: LAUNCH_BUDGET_MS,
      what: "demo.sh to notice magmux exited and say why",
      until: (_out, exited) => exited !== null,
    });
    demo = r.proc;
    check(
      "an explicitly requested layout is still built — with a warning that magmux may refuse it",
      /building it anyway/i.test(r.out) && /MAGMUX MAY REFUSE IT/.test(r.out),
      `the launcher said ${JSON.stringify(
        (/demo:rc: 24 rows[^\n]*/.exec(r.out)?.[0] ?? "(no warning at all)").slice(0, 150),
      )}`,
    );
    // THE ASSERTION THIS CHECK EXISTS FOR. magmux's own sentence, verbatim, in
    // front of the operator — plus the launcher saying out loud that the token
    // is a symptom, because that is the wrong trail this closes off.
    const reason = /cannot lay out \d+ panes in \d+x\d+[^\n]*/.exec(r.out)?.[0] ?? "";
    check(
      "the launcher stops and prints MAGMUX'S OWN REASON from magmux.err, not a missing-token message",
      r.exit !== 0 &&
        /magmux exited/.test(r.out) &&
        /magmux's own reason/.test(r.out) &&
        reason !== "" &&
        /SYMPTOM/i.test(r.out) &&
        !/cannot read the session token/.test(r.out),
      r.exit === 0
        ? `demo.sh exited 0 — it did not notice magmux had gone: ${JSON.stringify(r.out.slice(-200))}`
        : `exit=${r.exit}; verbatim from magmux.err: ${JSON.stringify(reason.slice(0, 150))}`,
    );
    // And it took its own litter with it: a launcher that dies mid-build still
    // owns the teardown, and the operator's pane goes back to full height.
    //
    // The throwaway server standing in for the operator's carries this run's id
    // in its own argv, and it is this check's scaffolding rather than the demo's
    // — `magmux-host-` is the name only it has, so dropping those lines asks the
    // question that was meant: did the DEMO leave anything behind.
    const left = () =>
      aliveWith(runID)
        .split("\n")
        .filter((l) => l.trim() !== "" && !l.includes("magmux-host-"))
        .join("\n")
        .trim();
    await until("the failed run to clean up after itself", 20_000, () => left() === "").catch(() => {});
    const panes = htmux("list-panes", "-t", `${hostSession}:workbench`, "-F", "#{pane_id} #{pane_height}")
      .out.trim()
      .split("\n")
      .filter(Boolean);
    check(
      "a run that dies this way still leaves no process, no $STATE and no pane of its own",
      left() === "" &&
        !fs.existsSync(runState) &&
        panes.length === 1 &&
        panes[0]!.startsWith(`${launcherPane} `),
      `survivors=${JSON.stringify(left().slice(0, 100))}; $STATE exists=${fs.existsSync(
        runState,
      )}; panes=[${panes.join(", ")}]`,
    );
  } catch (err: any) {
    check("the misattribution guard ran to completion", false, String(err?.message ?? err));
  } finally {
    demo?.kill("SIGTERM");
    spawnSync("bash", [path.join(HERE, "stop.sh")], {
      encoding: "utf8",
      env: { ...process.env, RC_DEMO_ID: runID, RC_DEMO_STATE: runState },
    });
    htmux("kill-session", "-t", hostSession);
    fs.rmSync(runState, { recursive: true, force: true });
    const dead = /no server running on (\S+)/.exec(htmux("list-sessions").out);
    if (dead) fs.rmSync(dead[1], { force: true });
  }
}

// ── check 14: THE DEFAULT SHAPE — magmux gets the window, at its real size ──
//
// The defect: a browser tab showing `pane 0 · 80×8`. Two causes, one of them in
// this file's own history and one in its proportions.
//
//   80 columns  a DETACHED tmux session has no client to take a size from and
//               defaults to 80x24, so a session created without -x/-y laid its
//               panes out at 80 columns whatever terminal it was started from.
//   8 rows      three surfaces shared the height evenly, so the thing being
//               DEMONSTRATED got the smallest share of it.
//
// Both are fixed by numbers rather than by intent, so both are asserted as
// numbers: the tmux pane heights out of `list-panes`, and — the one that cannot
// be faked by a tidy layout — the geometry magmux itself reports in the `watch`
// reply, which is exactly what the browser sizes itself from and prints in its
// window chrome. 200x50 in, `pane 0 · 200×34` out.
//
// BOTH MODES, because they resolve the size differently and only one of them
// was ever wrong: own-server mode from `tput` at session creation, adopt mode
// from `#{pane_width}`/`#{pane_height}` of the pane the launcher is in.
console.log(`\n${C.bold}  14 · the default shape: magmux gets the window, at the window's REAL size${C.reset}`);

/**
 * The geometry magmux reports for a pane, asked the way both clients ask.
 *
 * Through `watch` and not through `/v1/panes`, deliberately: the watch reply is
 * the message a client sizes itself from ("the client sizes itself HERE",
 * architecture.md), so this measures the number that reaches the browser rather
 * than an adjacent one that happens to agree.
 */
async function watchedGeometry(url: string, token: string, pane = 0): Promise<{ rows: number; cols: number }> {
  const ws = new WSClient(`${url.replace(/^http/, "ws")}/v1/ws`, token);
  await ws.open();
  try {
    const r: any = await ws.call("watch", { pane, mode: "frames", fps: 15 });
    return { rows: Number(r?.rows), cols: Number(r?.cols) };
  } finally {
    ws.close();
  }
}

/** Every pane of a demo's tmux session, as `id height` pairs, top to bottom. */
const paneHeights = (sock: string, target: string): { id: string; h: number }[] =>
  (spawnSync("tmux", ["-L", sock, "list-panes", "-a", "-t", target, "-F", "#{pane_id} #{pane_height}"], {
    encoding: "utf8",
  }).stdout ?? "")
    .trim()
    .split("\n")
    .filter(Boolean)
    .map((l) => ({ id: l.split(" ")[0]!, h: Number(l.split(" ")[1]) }));

/** The window both halves of check 14 use, and the numbers it must produce. */
const REAL_COLS = 200;
const REAL_ROWS = 50;
const WANT_DRIVER_ROWS = 14; // DRIVER_ROWS, demo.sh
const WANT_MAGMUX_ROWS = REAL_ROWS - 1 - WANT_DRIVER_ROWS; // everything left over: 35
const WANT_PANE_ROWS = WANT_MAGMUX_ROWS - 1; // magmux reserves one status row

if (!tmuxV) {
  console.log(`  ${C.yellow}SKIPPED${C.reset} ${C.grey}— tmux is not installed.${C.reset}`);
} else {
  // ── 14a: own-server mode ─────────────────────────────────────────────────
  const runID = `size-${process.pid}-${crypto.randomBytes(3).toString("hex")}`;
  const runState = `/tmp/mmrc-${runID}`;
  const tmuxSock = `magmux-${runID}`;
  const tmuxSession = `magmux-${runID}`;
  let demo: ChildProcess | null = null;
  try {
    const r = await runLauncher({
      env: {
        ...cleanDemoEnv(),
        NO_BROWSER: "1",
        NO_ATTACH: "1",
        RC_DEMO_TMUX: "own",
        RC_DEMO_ID: runID,
        RC_DEMO_STATE: runState,
        RC_DEMO_KEEP: "0",
        // The real terminal's size, which is what `tput` hands demo.sh from a
        // tty and what a pipe cannot be asked for. Pinned so the numbers below
        // mean the same thing on every machine.
        RC_DEMO_COLS: String(REAL_COLS),
        RC_DEMO_ROWS: String(REAL_ROWS),
      },
      budget: LAUNCH_BUDGET_MS,
      what: "demo.sh to build the default two-pane demo",
      until: (out, exited) => /holding the session open/.test(out) || exited !== null,
    });
    demo = r.proc;

    const panes = paneHeights(tmuxSock, tmuxSession);
    const magmuxPane = panes[0];
    const driverPane = panes[1];
    check(
      `the default layout is magmux + driver, and magmux gets the rows: ${WANT_MAGMUX_ROWS} against ${WANT_DRIVER_ROWS}`,
      panes.length === 2 &&
        magmuxPane!.h === WANT_MAGMUX_ROWS &&
        driverPane!.h === WANT_DRIVER_ROWS &&
        magmuxPane!.h > driverPane!.h,
      r.exit !== null
        ? `demo.sh exited ${r.exit}: ${JSON.stringify(r.out.trim().split("\n").slice(-4).join(" ⏎ ").slice(0, 200))}`
        : `list-panes in ${REAL_ROWS} rows: ${JSON.stringify(panes.map((p) => `${p.id} ${p.h}`))} ` +
          `(magmux first, then the driver)`,
    );

    // THE NUMBER THAT REACHES THE BROWSER. 80 columns here is the old defect
    // exactly: a session created without -x/-y, laid out at tmux's detached
    // default, and every client told the pane is 80 wide because it is.
    const url = fs.existsSync(path.join(runState, "magmux.url"))
      ? fs.readFileSync(path.join(runState, "magmux.url"), "utf8").trim()
      : "";
    const token = fs.existsSync(path.join(runState, "view.token"))
      ? fs.readFileSync(path.join(runState, "view.token"), "utf8").trim()
      : "";
    let geom: { rows: number; cols: number } | null = null;
    let geomErr = "";
    try {
      geom = await watchedGeometry(url, token);
    } catch (err: any) {
      geomErr = String(err?.message ?? err);
    }
    check(
      `own mode: the watch reply reports the REAL window — ${REAL_COLS}x${WANT_PANE_ROWS}, never 80x24`,
      geom?.cols === REAL_COLS && geom?.rows === WANT_PANE_ROWS,
      geom
        ? `watch reply: ${geom.cols}x${geom.rows} — the browser's chrome reads "pane 0 · ${geom.cols}×${geom.rows}"`
        : `no watch reply from ${url || "(no url)"}: ${geomErr}`,
    );

    demo.kill("SIGTERM");
    await until("the run to tear itself down", 30_000, () => !fs.existsSync(runState)).catch(() => {});
  } catch (err: any) {
    check("the default-shape check (own server) ran to completion", false, String(err?.message ?? err));
  } finally {
    demo?.kill("SIGTERM");
    spawnSync("bash", [path.join(HERE, "stop.sh")], {
      encoding: "utf8",
      env: { ...process.env, RC_DEMO_ID: runID, RC_DEMO_STATE: runState },
    });
    spawnSync("tmux", ["-L", tmuxSock, "kill-session", "-t", tmuxSession], { encoding: "utf8" });
    fs.rmSync(runState, { recursive: true, force: true });
    const dead = /no server running on (\S+)/.exec(
      spawnSync("tmux", ["-L", tmuxSock, "list-sessions"], { encoding: "utf8" }).stderr ?? "",
    );
    if (dead) fs.rmSync(dead[1]!, { force: true });
  }

  // ── 14b: adopt mode, where the size is a FACT rather than a guess ────────
  const aID = `size2-${process.pid}-${crypto.randomBytes(3).toString("hex")}`;
  const aState = `/tmp/mmrc-${aID}`;
  const hostSock = `magmux-host-${aID}`;
  const hostSession = "operator";
  const htmux = (...args: string[]) => {
    const r = spawnSync("tmux", ["-L", hostSock, ...args], { encoding: "utf8" });
    return { code: r.status ?? -1, out: `${r.stdout ?? ""}${r.stderr ?? ""}` };
  };
  let demo2: ChildProcess | null = null;
  try {
    htmux("new-session", "-d", "-s", hostSession, "-n", "workbench",
      "-x", String(REAL_COLS), "-y", String(REAL_ROWS), "sleep 600");
    const launcherPane = htmux("display-message", "-p", "-t", `${hostSession}:workbench`, "#{pane_id}").out.trim();
    const socketPath = htmux("display-message", "-p", "#{socket_path}").out.trim();
    const serverPID = htmux("display-message", "-p", "#{pid}").out.trim();

    const r = await runLauncher({
      env: {
        ...cleanDemoEnv(),
        TMUX: `${socketPath},${serverPID},0`,
        TMUX_PANE: launcherPane,
        NO_BROWSER: "1",
        RC_DEMO_ID: aID,
        RC_DEMO_STATE: aState,
        RC_DEMO_KEEP: "0",
      },
      budget: LAUNCH_BUDGET_MS,
      what: "the adopted default-shape demo to be ready",
      until: (out, exited) => /ready — the demo is running in this window/.test(out) || exited !== null,
    });
    demo2 = r.proc;

    // ONE pane created, not two, and the operator keeps the 14 rows the driver
    // needs — the driver IS their pane. Everything else went to magmux.
    const panes = htmux("list-panes", "-t", `${hostSession}:workbench`, "-F", "#{pane_id} #{pane_height}")
      .out.trim()
      .split("\n")
      .filter(Boolean)
      .map((l) => ({ id: l.split(" ")[0]!, h: Number(l.split(" ")[1]) }));
    check(
      `adopt mode: one pane created, magmux ${WANT_MAGMUX_ROWS} rows against the driver's ${WANT_DRIVER_ROWS}`,
      panes.length === 2 &&
        panes[0]!.h === WANT_MAGMUX_ROWS &&
        panes[1]!.id === launcherPane &&
        panes[1]!.h === WANT_DRIVER_ROWS,
      r.exit !== null
        ? `demo.sh exited ${r.exit}: ${JSON.stringify(r.out.trim().split("\n").slice(-4).join(" ⏎ ").slice(0, 200))}`
        : `panes in the operator's window: ${JSON.stringify(panes.map((p) => `${p.id} ${p.h}`))} ` +
          `(theirs is ${launcherPane})`,
    );

    const url = fs.existsSync(path.join(aState, "magmux.url"))
      ? fs.readFileSync(path.join(aState, "magmux.url"), "utf8").trim()
      : "";
    const token = fs.existsSync(path.join(aState, "view.token"))
      ? fs.readFileSync(path.join(aState, "view.token"), "utf8").trim()
      : "";
    let geom: { rows: number; cols: number } | null = null;
    let geomErr = "";
    try {
      geom = await watchedGeometry(url, token);
    } catch (err: any) {
      geomErr = String(err?.message ?? err);
    }
    check(
      `adopt mode: the watch reply reports the operator's OWN pane — ${REAL_COLS}x${WANT_PANE_ROWS}`,
      geom?.cols === REAL_COLS && geom?.rows === WANT_PANE_ROWS,
      geom ? `watch reply: ${geom.cols}x${geom.rows}` : `no watch reply from ${url || "(no url)"}: ${geomErr}`,
    );

    demo2.stdin?.write("q\n");
    await until("the adopted run to tear itself down", 60_000, () => !fs.existsSync(aState)).catch(() => {});
  } catch (err: any) {
    check("the default-shape check (adopt) ran to completion", false, String(err?.message ?? err));
  } finally {
    demo2?.kill("SIGTERM");
    spawnSync("bash", [path.join(HERE, "stop.sh")], {
      encoding: "utf8",
      env: { ...process.env, RC_DEMO_ID: aID, RC_DEMO_STATE: aState },
    });
    htmux("kill-session", "-t", hostSession);
    fs.rmSync(aState, { recursive: true, force: true });
    const dead = /no server running on (\S+)/.exec(htmux("list-sessions").out);
    if (dead) fs.rmSync(dead[1]!, { force: true });
  }
}

// ── check 15: RC_DEMO_GEOM — the escape hatch, and its refusals ─────────────
//
// The EXCEPTION, never the default and never chosen automatically: a terminal
// genuinely too small for a pane worth looking at, where no split can help.
// magmux then runs headless (`--headless` is forced when stdin is not a tty)
// at a size of its own, and there is no magmux pane at all.
//
// The refusal half matters as much as the mode: magmux does not FAIL on an
// out-of-range COLUMNS/LINES, it silently uses 80x24 and says so on a stderr
// this launcher redirects into a file — so an unvalidated value would look
// exactly like a demo that ignored what it was asked for.
console.log(`\n${C.bold}  15 · RC_DEMO_GEOM: a headless magmux at a size this window does not have${C.reset}`);

{
  // The refusals first: they start nothing at all, so they cost a process spawn
  // each and need no tmux.
  const bad = [
    { geom: "120x2", why: "under minPaneRows" },
    { geom: "10x36", why: "under minPaneCols" },
    { geom: "9000x36", why: "over the ceiling, where magmux would silently use 80x24" },
    { geom: "banana", why: "not a pair at all" },
  ];
  const results: { geom: string; code: number; out: string; state: boolean }[] = [];
  for (const b of bad) {
    const runState = `/tmp/mmrc-geom-${process.pid}-${b.geom.replace(/[^a-z0-9]/gi, "")}`;
    const p = spawnSync("bash", [path.join(HERE, "demo.sh")], {
      encoding: "utf8",
      env: {
        ...cleanDemoEnv(),
        NO_BROWSER: "1",
        NO_ATTACH: "1",
        RC_DEMO_TMUX: "own",
        RC_DEMO_GEOM: b.geom,
        RC_DEMO_ID: `geom-${process.pid}`,
        RC_DEMO_STATE: runState,
      },
    });
    results.push({
      geom: b.geom,
      code: p.status ?? -1,
      out: noAnsi(`${p.stdout ?? ""}${p.stderr ?? ""}`),
      state: fs.existsSync(runState),
    });
    fs.rmSync(runState, { recursive: true, force: true });
  }
  const allRefused = results.every(
    (r) => r.code === 1 && /RC_DEMO_GEOM=/.test(r.out) && /20 <= COLS <= 500/.test(r.out) && !r.state,
  );
  check(
    "an out-of-bounds RC_DEMO_GEOM is refused BEFORE anything starts, naming the bounds",
    allRefused,
    results
      .map((r) => `${r.geom} → exit ${r.code}${r.state ? " (LEFT A STATE DIR)" : ""}: ${
        JSON.stringify((/demo:rc: RC_DEMO_GEOM[^\n]*/.exec(r.out)?.[0] ?? r.out.slice(0, 60)).slice(0, 80))
      }`)
      .join("; "),
  );
}

if (!tmuxV) {
  console.log(`  ${C.yellow}SKIPPED${C.reset} ${C.grey}— tmux is not installed.${C.reset}`);
} else {
  const runID = `geom-${process.pid}-${crypto.randomBytes(3).toString("hex")}`;
  const runState = `/tmp/mmrc-${runID}`;
  const tmuxSock = `magmux-${runID}`;
  const tmuxSession = `magmux-${runID}`;
  let demo: ChildProcess | null = null;
  try {
    // A window that is 100x30 — a real one somebody might have — asking for a
    // 120x36 magmux, which is bigger than the window in BOTH dimensions and
    // therefore cannot be a pane of it.
    const r = await runLauncher({
      env: {
        ...cleanDemoEnv(),
        NO_BROWSER: "1",
        NO_ATTACH: "1",
        RC_DEMO_TMUX: "own",
        RC_DEMO_GEOM: "120x36",
        RC_DEMO_ID: runID,
        RC_DEMO_STATE: runState,
        RC_DEMO_KEEP: "0",
        RC_DEMO_COLS: "100",
        RC_DEMO_ROWS: "30",
      },
      budget: LAUNCH_BUDGET_MS,
      what: "demo.sh to build the headless demo",
      until: (out, exited) => /holding the session open/.test(out) || exited !== null,
    });
    demo = r.proc;

    // NO MAGMUX PANE. One tmux pane exists and it is the driver — which the
    // banner says too, because a human reading it must not be sent looking for
    // a pane that was never created.
    const panes = paneHeights(tmuxSock, tmuxSession);
    check(
      "RC_DEMO_GEOM creates NO magmux tmux pane — the driver is the only pane",
      panes.length === 1 &&
        /magmux is headless at 120x36/.test(r.out) &&
        /capture-pane -p -t %\d+\s+\(the driver\)/.test(r.out) &&
        !/capture-pane -p -t %\d+\s+\(magmux\)/.test(r.out),
      r.exit !== null
        ? `demo.sh exited ${r.exit}: ${JSON.stringify(r.out.trim().split("\n").slice(-4).join(" ⏎ ").slice(0, 200))}`
        : `panes=${JSON.stringify(panes.map((p) => `${p.id} ${p.h}`))}; the banner said ${JSON.stringify(
            (/\(no magmux pane\)[^\n]*/.exec(r.out)?.[0] ?? "(nothing about the missing pane)").slice(0, 90),
          )}`,
    );

    const url = fs.existsSync(path.join(runState, "magmux.url"))
      ? fs.readFileSync(path.join(runState, "magmux.url"), "utf8").trim()
      : "";
    const token = fs.existsSync(path.join(runState, "view.token"))
      ? fs.readFileSync(path.join(runState, "view.token"), "utf8").trim()
      : "";
    let geom: { rows: number; cols: number } | null = null;
    let geomErr = "";
    try {
      geom = await watchedGeometry(url, token);
    } catch (err: any) {
      geomErr = String(err?.message ?? err);
    }
    check(
      "…and the watch reply reports 120x36 — bigger than the 100x30 window it was started in",
      geom?.cols === 120 && geom?.rows === 36,
      geom
        ? `watch reply: ${geom.cols}x${geom.rows}, from a ${100}x${30} window`
        : `no watch reply from ${url || "(no url)"}: ${geomErr}`,
    );

    // The headless magmux is a CHILD OF THE LAUNCHER rather than a pane, so
    // nothing else can reap it: if the trap misses it, it survives the run
    // holding a port and a live feed of a shell.
    demo.kill("SIGTERM");
    await until("the headless run to tear itself down", 30_000, () => !fs.existsSync(runState)).catch(() => {});
    const left = aliveWith(runID);
    check(
      "the headless magmux is killed by the launcher's own trap — nothing survives it",
      left === "" && !fs.existsSync(runState),
      left === "" ? `nothing matches ${runID}; ${runState} removed` : `still running: ${JSON.stringify(left.slice(0, 160))}`,
    );
  } catch (err: any) {
    check("the RC_DEMO_GEOM check ran to completion", false, String(err?.message ?? err));
  } finally {
    demo?.kill("SIGTERM");
    spawnSync("bash", [path.join(HERE, "stop.sh")], {
      encoding: "utf8",
      env: { ...process.env, RC_DEMO_ID: runID, RC_DEMO_STATE: runState },
    });
    spawnSync("tmux", ["-L", tmuxSock, "kill-session", "-t", tmuxSession], { encoding: "utf8" });
    fs.rmSync(runState, { recursive: true, force: true });
    const dead = /no server running on (\S+)/.exec(
      spawnSync("tmux", ["-L", tmuxSock, "list-sessions"], { encoding: "utf8" }).stderr ?? "",
    );
    if (dead) fs.rmSync(dead[1]!, { force: true });
  }
}

// The DRIVER'S half of the same diagnosis, and it needs no tmux and no magmux:
// the driver is what an operator runs by hand against a demo somebody else
// started, so it meets this failure on its own, long after the launcher has
// gone. Given a state directory with magmux's last words in it and no token
// beside them, it must print those words FIRST — and it must keep the "is the
// demo running?" hint for the case where there is genuinely no evidence either
// way, which is the only case where that question is the right one.
//
// It runs the BINARY with no --run and no terminal, which is the shape that
// reaches the config loader before any of the three modes is chosen: the
// diagnosis has to happen before there is a TUI, a menu or an action, because
// it is the reason there will be none of them.
{
  const dir = fs.mkdtempSync("/tmp/mmrc-drive-");
  const empty = fs.mkdtempSync("/tmp/mmrc-drive-");
  try {
    fs.writeFileSync(path.join(dir, "magmux.url"), "http://127.0.0.1:52345\n");
    fs.writeFileSync(
      path.join(dir, "magmux.err"),
      "magmux: listening on http://127.0.0.1:52345\n" +
        "magmux: token in /tmp/x/magmux-x.token (mode 0600, removed at exit)\n" +
        "magmux: cannot lay out 1 panes in 80x2: every pane needs at least 3 rows and 20 columns\n",
    );
    const run = (state: string) => {
      const p = spawnSync(driverBin(), ["--state", state, "--id", "x"], { encoding: "utf8" });
      return { code: p.status ?? -1, out: noAnsi(`${p.stdout ?? ""}${p.stderr ?? ""}`) };
    };
    const withErr = run(dir);
    const without = run(empty);
    check(
      "the driver, given a missing token, reads magmux.err and reports MAGMUX'S reason first",
      withErr.code === 1 &&
        /magmux exited/.test(withErr.out) &&
        /cannot lay out 1 panes in 80x2/.test(withErr.out) &&
        /symptom/i.test(withErr.out) &&
        withErr.out.indexOf("cannot lay out") < withErr.out.indexOf("session token"),
      `exit=${withErr.code}: ${JSON.stringify(withErr.out.trim().split("\n").slice(0, 2).join(" ⏎ ").slice(0, 200))}`,
    );
    check(
      "…and it keeps 'is the demo running?' for the case with no evidence either way",
      without.code === 1 && /is the demo running/.test(without.out) && !/magmux exited/.test(without.out),
      `with an empty state directory it said ${JSON.stringify(
        without.out.trim().split("\n").slice(-1)[0]?.slice(0, 120) ?? "",
      )}`,
    );
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
    fs.rmSync(empty, { recursive: true, force: true });
  }
}

// ── the dependency guard: magmux's root module stays third-party-free ───────
//
// The driver is a Bubble Tea TUI, and Bubble Tea plus Lip Gloss plus Bubbles
// plus ntcharts is some twenty modules. magmux's root module has TWO
// dependencies — golang.org/x/sys for the PTY ioctls and golang.org/x/term for
// raw mode — and that is a load-bearing property of the project, not an
// accident: the control panel is raw ANSI written through magmux's own VT
// parser rather than through a TUI library precisely to keep it. So the driver
// is a SEPARATE Go module under demo/rc/driver with a `replace` back here, and
// this is the check that says so out loud.
//
// It asks the toolchain rather than reading go.mod, because `go list -m all`
// resolves the whole graph: a transitive dependency pulled in by something new
// would not appear in go.mod's require block but would appear here. A nested
// module is excluded from the parent's graph automatically, which is the whole
// mechanism this arrangement rests on — so if that ever stops being true, this
// check is what notices.
console.log(`\n${C.bold}  17 · the root module still has zero third-party dependencies${C.reset}`);
{
  const root = path.resolve(HERE, "../..");
  const mods = spawnSync("go", ["list", "-m", "all"], { cwd: root, encoding: "utf8" });
  const listed = (mods.stdout ?? "")
    .trim()
    .split("\n")
    .map((l) => l.split(" ")[0]!)
    .filter(Boolean);
  const want = ["github.com/MadAppGang/magmux", "golang.org/x/sys", "golang.org/x/term"];
  check(
    "`go list -m all` at the root is magmux, x/sys and x/term — and nothing else",
    listed.length === want.length && want.every((w) => listed.includes(w)),
    mods.status === 0
      ? `${listed.length} modules: ${listed.join(", ").slice(0, 200)}`
      : `go list failed: ${(mods.stderr ?? "").trim().slice(0, 160)}`,
  );
  // …and the nested module is genuinely invisible to the parent's package
  // pattern, which is what keeps `go test -race ./...` at the root unchanged.
  const pkgs = spawnSync("go", ["list", "./..."], { cwd: root, encoding: "utf8" });
  const nested = (pkgs.stdout ?? "").split("\n").filter((l) => l.includes("demo/rc/driver"));
  check(
    "…and ./... at the root does not descend into demo/rc/driver, so the suite is untouched",
    pkgs.status === 0 && nested.length === 0,
    nested.length === 0
      ? `${(pkgs.stdout ?? "").trim().split("\n").length} packages, none under demo/rc/driver`
      : `./... reached the nested module: ${nested.join(", ")}`,
  );
  // The driver's own module must still compile and vet, or the demo's front
  // door is broken in a way nothing else in this file would reach: every other
  // check here runs the BINARY, and a binary built yesterday runs fine.
  const vet = spawnSync("go", ["vet", "./..."], { cwd: path.join(HERE, "driver"), encoding: "utf8" });
  check(
    "the driver module builds and vets clean on its own",
    vet.status === 0,
    vet.status === 0
      ? "go vet ./... in demo/rc/driver is clean"
      : `go vet failed: ${`${vet.stdout ?? ""}${vet.stderr ?? ""}`.trim().slice(0, 240)}`,
  );
}

process.exit(report(checks));
