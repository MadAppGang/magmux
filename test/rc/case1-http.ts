#!/usr/bin/env bun
/**
 * RC case 1 — validation criterion 1: the HTTP surface, and its two refusals.
 *
 *   With the token:      list panes, open a pane, capture it, close it.
 *   Without the token:   401.
 *   With the view token:  403 on `input`.
 *
 * The two refusals are the point. A remote-control surface that works is easy;
 * one that refuses correctly is the whole security argument, and both refusals
 * have to be proved on the SAME running magmux as the happy path — a 401 from a
 * magmux that would have refused anything proves nothing.
 *
 * The full pane lifecycle is here rather than split across cases because
 * `close_pane` is the verb with a tombstone: the id it closes is never reused,
 * so the assertion is that the CLOSED pane leaves the live list while the ones
 * around it keep the indexes their caller already knows.
 */
import {
  C, RemoteMagmux, buildMagmux, criterion, dumpStderr, header, report, until,
  type Check,
} from "./harness.ts";

header(
  "RC case 1 — HTTP: the token, the 401 and the view token's 403",
  "validation criterion 1, against a real binary on a real TCP port",
);

const bin = buildMagmux();
const m = new RemoteMagmux(bin, "http");
const checks: Check[] = [];
let openedPane = -1;

try {
  // A view token as well as the session token, so both refusals are testable on
  // one process. `cat` blocks on its own PTY forever, so the session pane is
  // alive for the whole case with no clock in it.
  await m.start(["-e", "cat", "--label", "shell"], { viewToken: true });
  console.log(`\n${C.bold}  Setup${C.reset}`);
  console.log(`  ${C.grey}url        ${C.reset}${m.base}`);
  console.log(`  ${C.grey}token      ${C.reset}${m.token.slice(0, 8)}… (read off disk, ${m.token.length} chars)`);
  console.log(`  ${C.grey}view token ${C.reset}${m.viewToken.slice(0, 8)}…`);

  // ── with the token ───────────────────────────────────────────────────────
  console.log(`\n${C.bold}  With the session token${C.reset}`);

  const caps = await m.http("GET", "/v1/capabilities");
  checks.push({
    label: "GET /v1/capabilities answers 200",
    pass: caps.status === 200 && caps.json?.transports?.http?.listen !== undefined,
    detail: `${caps.status} transports.http.tls=${caps.json?.transports?.http?.tls} readOnly=${caps.json?.readOnly}`,
  });

  const list = await m.httpUntil("GET", "/v1/panes", (r) => r.status === 200 && (r.json?.panes?.length ?? 0) > 0);
  const sessionPanes = (list.json.panes as any[]).filter((p) => p.state !== "panel");
  checks.push({
    label: "GET /v1/panes lists the session",
    pass: sessionPanes.length === 1 && sessionPanes[0].label === "shell",
    detail: `${list.json.panes.length} entries, ${sessionPanes.length} session: ${JSON.stringify(sessionPanes.map((p) => [p.pane, p.label]))}`,
  });

  const opened = await m.http("POST", "/v1/ops/open_pane", {
    body: { cmd: "cat", label: "rc-http" },
  });
  openedPane = opened.json?.result?.pane ?? -1;
  checks.push({
    label: "POST /v1/ops/open_pane opens a pane",
    pass: opened.status === 200 && opened.json?.ok === true && openedPane >= 0,
    detail: `${opened.status} pane=${openedPane} ${opened.json?.result ? JSON.stringify(opened.json.result) : opened.text.trim()}`,
  });

  // The pane has to have painted something before a capture can show it. `cat`
  // prints nothing, so the marker is typed in and echoed back by the tty — which
  // is also the cheapest proof that the new pane has a live PTY behind it.
  await m.http("POST", "/v1/ops/input", { body: { pane: openedPane, text: "MAGMUX_HTTP_CAPTURE\r" } });
  let capture = await m.httpUntil(
    "GET",
    `/v1/panes/${openedPane}/screen`,
    (r) => r.status === 200 && String(r.json?.text ?? "").includes("MAGMUX_HTTP_CAPTURE"),
    20_000,
  );
  checks.push({
    label: "GET /v1/panes/{n}/screen captures the pane",
    pass: capture.status === 200 && String(capture.json?.text ?? "").includes("MAGMUX_HTTP_CAPTURE"),
    detail: `${capture.status} ${String(capture.json?.text ?? "").trim().split("\n").slice(-1)[0]?.slice(0, 40)}`,
  });

  const closed = await m.http("POST", "/v1/ops/close_pane", { body: { pane: openedPane } });
  checks.push({
    label: "POST /v1/ops/close_pane closes it",
    pass: closed.status === 200 && closed.json?.ok === true,
    detail: `${closed.status} ${JSON.stringify(closed.json?.result ?? closed.text.trim())}`,
  });

  let after: any[] = [];
  await until("the closed pane to leave the live list", 15_000, async () => {
    const r = await m.http("GET", "/v1/panes");
    after = r.json?.panes ?? [];
    return !after.some((p: any) => p.pane === openedPane && p.closed !== true);
  });
  checks.push({
    label: "the closed pane's id is a tombstone, not a renumbering",
    pass: sessionPanes.every((p) => after.some((q: any) => q.pane === p.pane && q.label === p.label)),
    detail: `surviving panes kept their ids: ${JSON.stringify(after.filter((p: any) => p.state !== "panel").map((p: any) => [p.pane, p.label]))}`,
  });

  // ── without a token ──────────────────────────────────────────────────────
  console.log(`\n${C.bold}  Without a credential${C.reset}`);

  const anon: Record<string, any> = {};
  for (const [label, call] of [
    ["GET /v1/panes", () => m.http("GET", "/v1/panes", { token: null })],
    ["GET /v1/capabilities", () => m.http("GET", "/v1/capabilities", { token: null })],
    ["POST /v1/ops/open_pane", () => m.http("POST", "/v1/ops/open_pane", { token: null, body: { cmd: "cat" } })],
    ["GET /v1/panes/0/screen", () => m.http("GET", "/v1/panes/0/screen", { token: null })],
    ["GET /v1/events", () => m.http("GET", "/v1/events", { token: null })],
  ] as [string, () => Promise<any>][]) {
    anon[label] = await call();
  }
  const anonStatuses = Object.entries(anon).map(([k, v]) => `${k}=${v.status}`);
  checks.push({
    label: "every endpoint returns 401 with no credential",
    pass: Object.values(anon).every((r: any) => r.status === 401),
    detail: anonStatuses.join(" "),
  });
  checks.push({
    label: "the 401 carries WWW-Authenticate and magmux's own code",
    pass:
      anon["GET /v1/panes"].headers.get("WWW-Authenticate")?.includes("Bearer") === true &&
      anon["GET /v1/panes"].json?.code === "unauthorized",
    detail: `${anon["GET /v1/panes"].headers.get("WWW-Authenticate")} code=${anon["GET /v1/panes"].json?.code}`,
  });

  const wrong = await m.http("GET", "/v1/panes", { token: "x".repeat(43) });
  checks.push({
    label: "a well-formed but wrong token is 401 too",
    pass: wrong.status === 401,
    detail: `${wrong.status} ${wrong.json?.error ?? ""}`,
  });

  // ── with the view token ──────────────────────────────────────────────────
  console.log(`\n${C.bold}  With the view token${C.reset}`);

  const viewList = await m.http("GET", "/v1/panes", { token: m.viewToken });
  checks.push({
    label: "a viewer may read: GET /v1/panes is 200",
    pass: viewList.status === 200 && (viewList.json?.panes?.length ?? 0) > 0,
    detail: `${viewList.status} ${viewList.json?.panes?.length} panes`,
  });

  const viewCaps = await m.http("GET", "/v1/capabilities", { token: m.viewToken });
  checks.push({
    label: "capabilities tells a viewer it is read-only",
    pass: viewCaps.status === 200 && viewCaps.json?.readOnly === true,
    detail: `${viewCaps.status} readOnly=${viewCaps.json?.readOnly}`,
  });

  const pane = sessionPanes[0].pane;
  const viewInput = await m.http("POST", "/v1/ops/input", {
    token: m.viewToken,
    body: { pane, text: "MAGMUX_VIEWER_SHOULD_NEVER_TYPE_THIS\r" },
  });
  checks.push({
    label: "a viewer is 403 on `input`",
    pass: viewInput.status === 403 && viewInput.json?.code === "forbidden",
    detail: `${viewInput.status} code=${viewInput.json?.code} ${String(viewInput.json?.error ?? "").slice(0, 60)}`,
  });

  for (const [op, body] of [
    ["send", { pane, text: "nope" }],
    ["open_pane", { cmd: "cat" }],
    ["close_pane", { pane }],
  ] as [string, any][]) {
    const r = await m.http("POST", `/v1/ops/${op}`, { token: m.viewToken, body });
    checks.push({
      label: `a viewer is 403 on \`${op}\``,
      pass: r.status === 403,
      detail: `${r.status} code=${r.json?.code}`,
    });
  }

  // The refusal has to be a refusal and not a delayed success: nothing a viewer
  // sent may reach the pane.
  //
  // A blank screen would satisfy that vacuously, so the full token types a
  // marker of its own first. The assertion is then the pair — the legitimate
  // marker IS on the screen, the viewer's is NOT — which cannot pass against a
  // capture that is empty or against one that is not being refreshed.
  await m.http("POST", "/v1/ops/input", { token: m.token, body: { pane, text: "MAGMUX_OWNER_TYPED_THIS\r" } });
  const screen = await m.httpUntil(
    "GET",
    `/v1/panes/${pane}/screen`,
    (r) => String(r.json?.text ?? "").includes("MAGMUX_OWNER_TYPED_THIS"),
    20_000,
    { token: m.viewToken },
  );
  const text = String(screen.json?.text ?? "");
  checks.push({
    label: "the owner's keystrokes reached the pane, the viewer's did not",
    pass: text.includes("MAGMUX_OWNER_TYPED_THIS") && !text.includes("MAGMUX_VIEWER_SHOULD_NEVER_TYPE_THIS"),
    detail: `${screen.status}, ${text.length} bytes of screen: owner marker present, viewer marker absent`,
  });
} catch (err) {
  checks.push({ label: "the case ran to completion", pass: false, detail: String(err) });
  dumpStderr(m);
} finally {
  await m.stop();
  m.cleanup();
}

const code = report(checks);
criterion(
  1,
  "HTTP: token works, no token is 401, view token is 403 on input",
  code === 0,
  `${checks.filter((c) => c.pass).length}/${checks.length} checks`,
);
process.exit(code);
