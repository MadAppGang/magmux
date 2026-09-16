#!/usr/bin/env bun
/**
 * RC case 4 — validation criterion 4: `magmux mcp` lists a pane as a resource,
 * and `resources/subscribe` yields `notifications/resources/updated` after the
 * pane is typed into.
 *
 * The typing is done over HTTP, not over MCP. That is the whole reason this
 * case exists beside the Go one: a change made through one transport has to be
 * observable through another, because the hub is supposed to be the single
 * place a session's state lives and each transport a view onto it. Using MCP's
 * own `send_keys` would prove the loop inside one process and say nothing about
 * that.
 *
 * `magmux mcp` is a SEPARATE PROCESS that finds magmux by discovery in
 * MAGMUX_SOCK_DIR — no socket path is handed to it — and every line it writes
 * is checked as it is read, so one stray print anywhere in the binary fails
 * this case.
 */
import {
  C, RemoteMagmux, buildMagmux, criterion, dumpStderr, header, report, type Check,
} from "./harness.ts";
import { MCPChild, contentsText, resourceURIs, toolNames } from "./mcpclient.ts";

const MARKER = "MAGMUX_MCP_RC_OK";
const SESSION = `rc4-${process.pid}`;

header(
  "RC case 4 — MCP: panes as resources, and subscribe → updated",
  "validation criterion 4; the typing crosses HTTP, the notification comes back over MCP",
);

const bin = buildMagmux();
const m = new RemoteMagmux(bin, "mcp");
const checks: Check[] = [];
// Built after magmux starts, because the directory it discovers magmux in is
// the one magmux bound its socket in.
let mcp!: MCPChild;

try {
  await m.start(["-e", "sh", "--label", "shell", "--id", SESSION]);
  mcp = new MCPChild(bin, m.sockDir);
  console.log(`\n${C.bold}  Setup${C.reset}`);
  console.log(`  ${C.grey}session  ${C.reset}${SESSION}`);
  console.log(`  ${C.grey}sock dir ${C.reset}${m.sockDir}  ${C.grey}(magmux mcp discovers it; no path is handed over)${C.reset}`);

  mcp.start();
  const t0 = Date.now();
  const init = await mcp.handshake("magmux-rc/case4");
  console.log(`  ${C.grey}initialize ${C.reset}${Date.now() - t0}ms — ${init?.serverInfo?.name} ${init?.serverInfo?.version}`);

  checks.push({
    label: "the server advertises resource subscription",
    pass: init?.capabilities?.resources?.subscribe === true && init?.capabilities?.resources?.listChanged === true,
    detail: JSON.stringify(init?.capabilities?.resources),
  });

  // ── resources/list ───────────────────────────────────────────────────────
  const paneURI = `magmux://${SESSION}/pane/0/screen`;
  let uris: string[] = [];
  const listDeadline = Date.now() + 30_000;
  while (Date.now() < listDeadline) {
    uris = resourceURIs(await mcp.call("resources/list"));
    if (uris.includes(paneURI)) break;
    await new Promise((r) => setTimeout(r, 200));
  }
  checks.push({
    label: "resources/list carries magmux://{session}/pane/{n}/screen",
    pass: uris.includes(paneURI),
    detail: uris.length ? uris.join(" ") : "(none)",
  });

  const templates = await mcp.call("resources/templates/list");
  const tmpl = (templates?.resourceTemplates ?? []).map((t: any) => t.uriTemplate);
  checks.push({
    label: "the URI template is published, so a client can address a pane it has not seen",
    pass: tmpl.includes("magmux://{session}/pane/{pane}/screen"),
    detail: tmpl.join(" ") || "(none)",
  });

  const tools = toolNames(await mcp.call("tools/list"));
  checks.push({
    label: "the static tools are there",
    pass: ["list_sessions", "list_panes", "open_pane", "close_pane", "read_pane", "send_keys"].every((t) => tools.includes(t)),
    detail: `${tools.length} tools: ${tools.slice(0, 8).join(" ")}…`,
  });

  // ── subscribe, then type over HTTP ───────────────────────────────────────
  await mcp.call("resources/subscribe", { uri: paneURI });
  console.log(`\n${C.bold}  subscribe → type over HTTP → updated${C.reset}`);

  // Let the shell settle so the notification measured below is caused by this
  // case's own keystrokes rather than by the prompt arriving late. Drain first,
  // then require quiet: a poll, not a sleep sized to this machine.
  await drainUpdates(mcp, paneURI, 700, 15_000);

  // The mark is taken BEFORE the write and the wait starts from it, so a
  // notification produced by the prompt settling cannot be mistaken for one
  // produced by these keystrokes.
  const before = mcp.mark;
  const started = Date.now();
  const typed = await m.http("POST", "/v1/ops/input", { body: { pane: 0, text: `echo ${MARKER}\r` } });
  checks.push({
    label: "the keystrokes went in over HTTP, not over MCP",
    pass: typed.status === 200 && typed.json?.ok === true,
    detail: `POST /v1/ops/input → ${typed.status} ${JSON.stringify(typed.json?.result)}`,
  });

  const note = await mcp.await(
    "notifications/resources/updated for the pane",
    30_000,
    (msg) => msg.method === "notifications/resources/updated" && msg.params?.uri === paneURI,
    before,
  );
  const noteMs = Date.now() - started;
  checks.push({
    label: "resources/subscribe yields notifications/resources/updated",
    pass: mcp.messages.indexOf(note) >= before,
    detail: `${noteMs}ms after the HTTP write (message #${mcp.messages.indexOf(note)}, mark was #${before}), uri=${note.params.uri}`,
  });
  checks.push({
    label: "the notification carries a uri and nothing else",
    pass: Object.keys(note.params ?? {}).length === 1 && typeof note.params.uri === "string",
    detail: JSON.stringify(note.params),
  });

  const screen = contentsText(await mcp.call("resources/read", { uri: paneURI }));
  checks.push({
    label: "reading the resource shows what HTTP typed",
    pass: screen.includes(MARKER),
    detail: screen.trim().split("\n").slice(-1)[0]?.slice(0, 50) ?? "(empty)",
  });

  // ── a pane that does not exist is -32002, not a crash ────────────────────
  const missing = mcp.request("resources/read", { uri: `magmux://${SESSION}/pane/99/screen` });
  const err = await mcp.await("the error for a missing pane", 20_000, (x) => x.id === missing);
  checks.push({
    label: "an unknown pane is MCP's -32002, so a client re-lists rather than retrying",
    pass: err?.error?.code === -32002,
    detail: `code=${err?.error?.code} ${String(err?.error?.message ?? "").slice(0, 60)}`,
  });

  // ── stdout hygiene, on the real stream ───────────────────────────────────
  //
  // Stated against the number of requests this client issued rather than a
  // round number: every request is owed a response, so the floor moves with the
  // case instead of being a threshold somebody has to remember to raise.
  const responses = mcp.messages.filter((x) => x.id !== undefined).length;
  checks.push({
    label: "every line on the child's stdout was JSON-RPC, and every request was answered",
    pass: mcp.stray.length === 0 && responses >= mcp.requestCount,
    detail:
      `${mcp.messages.length} JSON-RPC messages (${responses} responses to ${mcp.requestCount} requests, ` +
      `${mcp.messages.length - responses} notifications), ${mcp.stray.length} stray lines` +
      (mcp.stray.length ? `: ${mcp.stray[0].slice(0, 60)}` : ""),
  });
} catch (err) {
  checks.push({ label: "the case ran to completion", pass: false, detail: String(err) });
  dumpStderr(m);
  if (mcp?.stderr) console.log(`\n  ${C.grey}magmux mcp stderr: ${mcp.stderr.slice(-400)}${C.reset}`);
} finally {
  mcp?.stop();
  await m.stop();
  m.cleanup();
}

const code = report(checks);
criterion(
  4,
  "MCP: pane resources listed, subscribe yields resources/updated after typing",
  code === 0,
  `${checks.filter((c) => c.pass).length}/${checks.length} checks`,
);
process.exit(code);

/** Wait until no `updated` for `uri` has arrived for `idleMs`. A poll, not a sleep. */
async function drainUpdates(mcp: MCPChild, uri: string, idleMs: number, within: number) {
  const deadline = Date.now() + within;
  let lastSeen = Date.now();
  let count = mcp.messages.filter((x) => x.method === "notifications/resources/updated" && x.params?.uri === uri).length;
  while (Date.now() < deadline) {
    const now = mcp.messages.filter((x) => x.method === "notifications/resources/updated" && x.params?.uri === uri).length;
    if (now !== count) {
      count = now;
      lastSeen = Date.now();
    } else if (Date.now() - lastSeen >= idleMs) {
      return;
    }
    await new Promise((r) => setTimeout(r, 25));
  }
}
