#!/usr/bin/env bun
/**
 * The loopback static server for the browser half of the demo.
 *
 *   bun demo/rc/serve.ts --state $STATE
 *   RC_DEMO_STATE=$STATE bun demo/rc/serve.ts
 *
 * magmux serves no static files and adding a route would be a product change
 * this demo is forbidden to make (`mux/remote.go:155-161`), so the page needs a
 * server of its own. It is a hundred lines of `Bun.serve`, and it buys the one
 * thing a `file://` page cannot have: a real HTTP ORIGIN, which the launcher
 * hands to magmux's `--allow-origin`. A `file://` page sends `Origin: null`,
 * and allowing that allows a CLASS — every sandboxed iframe and every `data:`
 * document sends the same thing.
 *
 * Having an origin is also what lets the page GET its credential instead of
 * being given one: the config is same-origin, so the token never goes in a URL,
 * a fragment or a paste buffer.
 *
 * ── THE DRIVER ENDPOINT, AND THE ESCALATION IT IS ──────────────────────────
 *
 * The page holds the READ-ONLY view token and that is the whole of its security
 * claim: a page left open in a browser provably cannot type into a shell. The
 * driver's actions need the FULL SESSION TOKEN, so the page is NOT given one.
 * Instead THIS server holds both tokens and performs an allow-listed action on
 * the page's behalf, returning magmux's own status, body and timing unchanged.
 *
 * SAY IT PLAINLY, BECAUSE IT IS A REAL ESCALATION AND IT IS EASY TO MISS:
 * without this endpoint, stealing the page's URL buys a read-only video feed of
 * a shell. WITH it, it buys the ability to TYPE INTO that shell. The guards are
 * the same two the config endpoint already has — an unguessable per-run path
 * and `Sec-Fetch-Site: same-origin` — and the prize behind them is now much
 * bigger. `RC_DEMO_WEB_DRIVER=0` removes the endpoint entirely (no path is
 * minted, no action list is served, the page hides the panel) and leaves the
 * page purely read-only.
 *
 * Two properties keep this from being a general proxy, and both are load
 * bearing rather than defensive habit:
 *
 *   1. It is an ALLOW-LIST of NAMED actions. `ACTIONS` below is the whole of
 *      what this endpoint can ask magmux to do; the op, the method, the path,
 *      the credential and every argument are written HERE. "Forward this
 *      request to magmux" is a much larger thing than a demo should ship, and
 *      it would hand the page the session token in all but name.
 *   2. The only value a request may carry besides the action name is an INTEGER
 *      PANE ID, validated as one. The page already knows every pane id from its
 *      own read-only aggregate, so it is telling this server which pane it is
 *      looking at and nothing more.
 *
 * Refusals this server decides itself carry `"by": "serve.ts"` and never an
 * `upstream` field, so the panel can never print one of them as though magmux
 * had said it. The panel must never invent a response.
 *
 * THE ORDER BELOW IS A SECURITY ORDER, and it is magmux's own
 * (`mux/remote.go:16-24`): mint the credentials and write the token at 0600;
 * build the bundle; and only THEN bind. So an accepting port implies a final
 * token file at its final name with its final bytes, which is what makes the
 * single `ready {...}` line a readiness signal a launcher can trust rather than
 * a hint. Every failure before the bind takes the token file with it — a file
 * naming a credential for a port nothing is listening on is litter that looks
 * like a secret.
 */
import fs from "node:fs";
import path from "node:path";
import process from "node:process";

const HERE = import.meta.dir;
const REPO = path.resolve(HERE, "../..");

interface Args {
  /** The run's state directory: the token, the url file, the logs. */
  state: string;
  tokenFile: string;
  /** This run's id, which names magmux's own token file. */
  id: string;
  /** Where magmux's SESSION token is. Resolved lazily; see sessionToken(). */
  sessionTokenFile: string;
  pane: number | null;
  host: string;
  port: number;
}

function parseArgs(argv: string[]): Args {
  const a: Args = {
    state: process.env.RC_DEMO_STATE || path.join(REPO, ".task-grids/rc-demo"),
    tokenFile: "",
    id: process.env.RC_DEMO_ID || "",
    sessionTokenFile: "",
    pane: null,
    host: "127.0.0.1",
    port: 0,
  };
  for (let i = 0; i < argv.length; i++) {
    switch (argv[i]) {
      case "--state":
        a.state = argv[++i] ?? "";
        break;
      case "--token-file":
        a.tokenFile = argv[++i] ?? "";
        break;
      case "--id":
        a.id = argv[++i] ?? "";
        break;
      case "--session-token-file":
        a.sessionTokenFile = argv[++i] ?? "";
        break;
      case "--pane":
        a.pane = Number(argv[++i]);
        break;
      case "--port":
        a.port = Number(argv[++i]);
        break;
      case "--help":
      case "-h":
        process.stdout.write(
          "bun demo/rc/serve.ts [--state DIR] [--token-file PATH] [--id ID]\n" +
            "                     [--session-token-file PATH] [--pane N] [--port N]\n" +
            "  --state defaults to $RC_DEMO_STATE, then <repo>/.task-grids/rc-demo\n" +
            "  RC_DEMO_WEB_DRIVER=0 removes the driver endpoint and hides the page's panel\n",
        );
        process.exit(0);
      default:
        die(`unknown argument ${argv[i]}`);
    }
  }
  if (!a.state) die("--state (or $RC_DEMO_STATE) is required");
  a.state = path.resolve(a.state);
  if (!a.tokenFile) a.tokenFile = path.join(a.state, "view.token");
  if (!a.sessionTokenFile && a.id) a.sessionTokenFile = path.join(a.state, `magmux-${a.id}.token`);
  return a;
}

function die(msg: string): never {
  process.stderr.write(`serve: ${msg}\n`);
  process.exit(1);
}

/**
 * 32 random bytes as unpadded base64url — 43 characters of [A-Za-z0-9_-].
 *
 * This is magmux's own token alphabet, and the shape is not cosmetic:
 * `auth.LoadFile` refuses anything else, so a token minted in another alphabet
 * makes magmux refuse to start with a message about the file rather than about
 * this script. The same generator mints the config path's secret, because the
 * two want the same property — unguessable, and safe in a URL.
 */
function randomB64(bytes = 32): string {
  const raw = new Uint8Array(bytes);
  crypto.getRandomValues(raw);
  return Buffer.from(raw).toString("base64url");
}

/**
 * Write a token the way magmux insists on finding one: a regular file, owned
 * by this euid, 0600 and nothing wider. The chmod after the write is belt and
 * braces against a umask a `mode:` alone does not fully defeat.
 */
function writeTokenFile(file: string, token: string) {
  fs.mkdirSync(path.dirname(file), { recursive: true });
  fs.writeFileSync(file, token + "\n", { mode: 0o600 });
  fs.chmodSync(file, 0o600);
}

const args = parseArgs(process.argv.slice(2));
fs.mkdirSync(args.state, { recursive: true });

/**
 * The file the launcher writes magmux's URL into.
 *
 * It is read PER REQUEST and never cached, because of an ordering this demo
 * cannot escape: this server has to bind FIRST — magmux's `--allow-origin` must
 * name a port the kernel has not chosen yet — so magmux's own address cannot
 * exist here at startup. The launcher writes this file the instant its
 * readiness poll matches `magmux: listening on (\S+)`.
 */
const URL_FILE = path.join(args.state, "magmux.url");

// ── 1. the credentials ──────────────────────────────────────────────────────

const viewToken = randomB64();
writeTokenFile(args.tokenFile, viewToken);

/**
 * The config lives at an unguessable per-run path, not at `/config.json`.
 *
 * The response carries the view token, and the view token is a live video feed
 * of a shell somebody is typing into. At a fixed path, ANY process on the
 * machine as ANY user could `curl` it and have that feed — a strictly weaker
 * gate than the 0600 the token file itself is held to, which would make the
 * file permission theatre. Two cheap defences, because either alone fails open:
 *
 *   1. this secret path, which is in the URL the launcher opens and therefore
 *      in the page's own location, and nowhere else;
 *   2. `Sec-Fetch-Site: same-origin` (or a self Origin) on the request, which
 *      every modern browser sends on a same-origin fetch and which `curl`
 *      does not send at all unless it is told to.
 *
 * And, as ever, no `Access-Control-Allow-Origin` on the response: another
 * origin may make the request; it must not be able to read the answer.
 */
const configPath = `/c/${randomB64(24)}.json`;

/**
 * The driver endpoint, on its own unguessable per-run path — or nothing at all.
 *
 * `RC_DEMO_WEB_DRIVER=0` is the switch for anyone who wants the page to stay
 * purely read-only: no path is minted, the config carries no `driver` field, the
 * page hides its panel, and there is no route to find. Anything else leaves the
 * endpoint on, which is the demo's default because the panel IS half of what
 * batch 7 exists to show.
 *
 * It is minted with the same generator as the config path and for the same
 * reason. It is NOT the same path, so a config URL that somehow leaks — a
 * screenshot of the address bar, a shell history — does not also name the
 * endpoint that can type.
 */
const driverEnabled = (process.env.RC_DEMO_WEB_DRIVER ?? "1") !== "0";
const driverPath = driverEnabled ? `/d/${randomB64(24)}.json` : "";

// From here on, any failure must take the token file with it.
function abort(msg: string): never {
  try {
    fs.rmSync(args.tokenFile, { force: true });
  } catch {}
  process.stderr.write(`serve: ${msg}\n`);
  process.exit(1);
}

// ── 2. the bundle ───────────────────────────────────────────────────────────

/**
 * The page really runs the shared source.
 *
 * `web/app.ts` imports `../client.ts`, which imports `../frame.ts` — the same
 * files `cli.ts` imports — and Bun.build resolves them here rather than a
 * second copy being kept in step by discipline. That is the whole claim of the
 * demo made structural: if the decoder changes, both clients change.
 *
 * It happens BEFORE the bind and before tmux exists, so a build error is a
 * message on a clean terminal rather than a blank pane inside a layout.
 */
const built = await Bun.build({
  entrypoints: [path.join(HERE, "web/app.ts")],
  target: "browser",
  minify: false,
  sourcemap: "none",
});
if (!built.success || built.outputs.length === 0) {
  const logs = built.logs.map((l) => String(l)).join("\n");
  abort(`Bun.build of web/app.ts failed:\n${logs}`);
}
const appJS = await built.outputs[0].text();
const indexHTML = fs.readFileSync(path.join(HERE, "web/index.html"), "utf8");

// ── 3. the bind ─────────────────────────────────────────────────────────────

function magmuxURL(): string {
  try {
    return fs.readFileSync(URL_FILE, "utf8").trim();
  } catch {
    return "";
  }
}

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json", "Cache-Control": "no-store" },
  });
}

/**
 * Is this request one this page made of itself?
 *
 * `Sec-Fetch-Site` is set by the BROWSER and cannot be forged by page script,
 * which is the whole reason it is worth checking; `Origin` covers the older
 * browser that does not send it. Neither is authentication — the secret path
 * and the 0600 token file are — and both are here because the thing being
 * served is a credential and a curl from another shell on this machine should
 * not get one.
 */
function sameOrigin(req: Request, port: number): boolean {
  const site = req.headers.get("sec-fetch-site");
  if (site === "same-origin") return true;
  const origin = req.headers.get("origin") ?? "";
  return origin === `http://127.0.0.1:${port}` || origin === `http://localhost:${port}`;
}

// ── the driver's allow-list ─────────────────────────────────────────────────

/**
 * One permitted action. The op, the method, the path, the credential and every
 * argument are written HERE — a request names an action and, at most, a pane.
 */
interface Action {
  /** What a request names, and what the panel's button carries. */
  name: string;
  /** The driver action this is the browser's copy of, so the two can be compared. */
  item: string;
  /** Set on the FIRST action of a group, exactly as the driver sets it. */
  group?: string;
  title: string;
  /** The one-line "what it really does", in the driver's own words. */
  how: string;
  method: "GET" | "POST";
  /** magmux's path. FIXED; never taken from the request. */
  path: string;
  /** Which token this server spends. The contrast is the point of items 8 and 9. */
  cred: "session" | "view";
  /** Does this action need the page's pane id? "target" = the watched pane. */
  pane: "target" | "none";
  /**
   * Resolve the pane HERE instead, by asking magmux.
   *
   * "opened" is the newest pane labelled `driver`, i.e. the one the open-pane
   * action made. Closing the pane the page is WATCHING would be the other
   * reading of item 7 and it is the wrong one: the page would lose its own
   * stream to `pane_closed` and end, which teaches a viewer that the button is
   * broken rather than that an id is a tombstone.
   */
  resolve?: "opened";
  /** The body, built here, from nothing but the validated pane id. */
  body?: (pane: number) => Record<string, unknown>;
  /** Where to look for the effect — the driver's rule 2, kept on this side too. */
  look: string;
}

/**
 * Everything the page may ask for. Thirteen items in the driver, eleven here.
 *
 * Omitted, and why:
 *
 *   item 11 (stall a WebSocket client) — it needs a socket that HANDSHAKES and
 *     then stops reading. A browser's WebSocket always drains its own receive
 *     buffer and offers no way to ask it not to, so a page cannot stage this
 *     failure at all; the driver stages it with a real socket it stops reading.
 *   item 12 (kill magmux) — it is not an HTTP op. It is a SIGKILL sent to a pid
 *     found by argv, i.e. exactly the "run something on this machine" power
 *     this endpoint is built to NOT have. A page that can kill a process is a
 *     far larger escalation than one that can type into a shell, and it would
 *     end the run the page is watching.
 *
 * Item 6 (point the mirrors at a pane) is here as `panes`, printing the list,
 * because the retargeting itself is the page's OWN act — the pane picker in the
 * status bar, `unwatch` then `watch` on its own read-only connection. There is
 * no op that points somebody else's stream somewhere new, and inventing a side
 * channel for one would contradict the API this demo exists to show.
 * Item 13 (the raw frame) is done in the PAGE with the view token, for the same
 * reason: it needs no escalation, so it must not have one.
 */
const ACTIONS: Action[] = [
  {
    name: "type",
    item: "1",
    group: "TYPE INTO THE SESSION",
    title: "type a command",
    how: "`ls --color=auto`, one POST /v1/ops/input",
    method: "POST",
    path: "/v1/ops/input",
    cred: "session",
    pane: "target",
    body: (pane) => ({ pane, text: "ls --color=auto", keys: ["enter"] }),
    look: "this page and the terminal mirror — the same colours, decoded from the same frame",
  },
  {
    name: "burst",
    item: "2",
    title: "a burst of scrolling output",
    how: "200 lines as fast as the shell can print them",
    method: "POST",
    path: "/v1/ops/input",
    cred: "session",
    pane: "target",
    body: (pane) => ({
      pane,
      text: "i=1; while [ $i -le 200 ]; do printf '%3d  row deltas, not a screenshot\\n' $i; i=$((i+1)); done",
      keys: ["enter"],
    }),
    look: "the mirrors keep up because magmux sends CHANGED ROWS, not screens — watch seq jump",
  },
  {
    name: "top",
    item: "3",
    title: "run `top`",
    how: "the ALT SCREEN — magmux forces a keyframe across the switch",
    method: "POST",
    path: "/v1/ops/input",
    cred: "session",
    pane: "target",
    body: (pane) => ({ pane, text: "top", keys: ["enter"] }),
    look: "the status bar above now says `alt screen`",
  },
  {
    name: "quit-top",
    item: "4",
    title: "quit top",
    how: "the letter q, straight at the PTY — another forced keyframe",
    method: "POST",
    path: "/v1/ops/input",
    cred: "session",
    pane: "target",
    body: (pane) => ({ pane, keys: ["q"] }),
    look: "the primary screen returns with its scrollback intact",
  },
  {
    name: "open-pane",
    item: "5",
    group: "PANES",
    title: "open a second pane",
    how: "POST /v1/ops/open_pane — magmux splits its own layout",
    method: "POST",
    path: "/v1/ops/open_pane",
    cred: "session",
    pane: "none",
    body: () => ({
      cmd: "date; echo 'this pane was opened from the browser panel, over POST /v1/ops/open_pane'; exec ${SHELL:-/bin/sh} -i",
      label: "driver",
      split: "auto",
    }),
    look: "the new pane appears in the picker top right — pick it to watch it",
  },
  {
    name: "panes",
    item: "6",
    title: "list the panes",
    how: "GET /v1/panes — then retarget with the picker, which is this page's own act",
    method: "GET",
    path: "/v1/panes",
    cred: "session",
    pane: "none",
    look: "a watch is per-SUBSCRIBER state; no op retargets somebody else's stream",
  },
  {
    name: "close-pane",
    item: "7",
    title: "close the pane that action opened",
    how: "close_pane on the newest `driver` pane — an id slot is a tombstone",
    method: "POST",
    path: "/v1/ops/close_pane",
    cred: "session",
    pane: "none",
    resolve: "opened",
    body: (pane) => ({ pane }),
    look: "the picker loses it, and the next pane takes the NEXT id, never the freed one",
  },
  {
    name: "refuse-input",
    item: "8",
    group: "WHAT A VIEWER CANNOT DO",
    title: "try `input` with the VIEW token",
    how: "the credential THIS PAGE holds. magmux answers, not this server",
    method: "POST",
    path: "/v1/ops/input",
    cred: "view",
    pane: "target",
    body: (pane) => ({ pane, text: "rm -rf /" }),
    look: "403 forbidden, off the wire — nothing was typed into any pane",
  },
  {
    name: "refuse-open",
    item: "9",
    title: "try `open_pane` with the VIEW token",
    how: "same credential, a control-class op this time",
    method: "POST",
    path: "/v1/ops/open_pane",
    cred: "view",
    pane: "none",
    body: () => ({ cmd: "sleep 60" }),
    look: "class control this time, class input last time — one rule, not a list of blocked ops",
  },
  {
    name: "view-panes",
    item: "9b",
    title: "…and the same VIEW token on a read op",
    how: "GET /v1/panes with the view token — a capability, not a ban",
    method: "GET",
    path: "/v1/panes",
    cred: "view",
    pane: "none",
    look: "that 200 is why this page can watch at all: `watch` and `list` are read-class",
  },
  {
    name: "ticket",
    item: "10",
    group: "THE PLUGIN",
    title: "run a ticket",
    how: "ticket.run_ticket — an op no line of magmux knows about",
    method: "POST",
    path: "/v1/ops/ticket.run_ticket",
    cred: "session",
    pane: "none",
    body: () => ({ title: "mirror this ticket end to end" }),
    look: "a pane opens and the PLUGIN drives it; poll it with the next action",
  },
  {
    name: "ticket-status",
    item: "10b",
    title: "poll the ticket",
    how: "ticket.status — the plugin's own observation of its own pane",
    method: "POST",
    path: "/v1/ops/ticket.status",
    cred: "session",
    pane: "none",
    body: () => ({}),
    look: "state goes sent → done; that answer is a controller snapshot, not a guess",
  },
];

/**
 * magmux's SESSION token, read PER REQUEST and never held in a variable.
 *
 * Per request for the same reason `magmux.url` is: this server binds before
 * magmux exists, so at startup there is no token to read. Re-reading also means
 * the escalation lasts exactly as long as magmux does — when the launcher
 * removes the token file at exit, this endpoint stops being able to type before
 * anything else notices.
 *
 * Resolution order: `--session-token-file`, then `{state}/magmux-{id}.token`
 * from `--id`, then the one `magmux-*.token` in the state directory. The last
 * is for a magmux started WITHOUT `--id`, whose token file is named after its
 * pid — which cannot be known before it runs. More than one match is refused
 * rather than guessed: spending the wrong session token is not a thing to do on
 * a coin toss.
 */
function sessionToken(): { token: string; from: string } | { error: string } {
  const candidates: string[] = [];
  if (args.sessionTokenFile) candidates.push(args.sessionTokenFile);
  else {
    let names: string[] = [];
    try {
      names = fs.readdirSync(args.state).filter((f) => /^magmux-[^/]+\.token$/.test(f));
    } catch {}
    if (names.length > 1) {
      return { error: `${names.length} token files in ${args.state}; name one with --id or --session-token-file` };
    }
    for (const n of names) candidates.push(path.join(args.state, n));
  }
  for (const f of candidates) {
    try {
      const v = fs.readFileSync(f, "utf8").trim();
      if (v) return { token: v, from: f };
    } catch {}
  }
  return {
    error:
      "magmux's session token is not readable yet — it writes that file before it binds " +
      "and removes it at exit, so this means magmux is not running",
  };
}

/**
 * The newest pane labelled `driver` — the one the open-pane action made.
 *
 * HIGHEST id, because ids only ever go up and are never reused, so the newest
 * such pane is the one that action opened most recently. Asking magmux rather
 * than remembering is deliberate: this server is restartable and two pages may
 * be open at once, and a remembered id would be a second, quieter copy of
 * magmux's own pane table waiting to disagree with it.
 */
async function openedPane(magmux: string, token: string): Promise<{ pane: number } | { error: string }> {
  let res: Response;
  try {
    res = await fetch(`${magmux}/v1/panes`, { headers: { Authorization: `Bearer ${token}` } });
  } catch (err: any) {
    return { error: `could not list panes: ${err?.message ?? err}` };
  }
  if (!res.ok) return { error: `magmux answered ${res.status} to GET /v1/panes` };
  const body: any = await res.json().catch(() => null);
  const mine = (body?.panes ?? [])
    .filter((p: any) => String(p?.label ?? "") === "driver" && p?.state !== "closed")
    .map((p: any) => Number(p.pane))
    .filter((n: number) => Number.isInteger(n));
  if (!mine.length) return { error: 'no pane is labelled "driver" — run the open-pane action first' };
  return { pane: Math.max(...mine) };
}

/**
 * Run one allow-listed action against magmux and report exactly what came back.
 *
 * The response carries magmux's own STATUS, so the panel shows the real 403s,
 * and an `upstream` object with the request as sent, the body as received and
 * the latency. Nothing here rewrites a body, invents a message or decides that
 * a refusal was expected: the panel must never invent a response.
 */
async function runAction(act: Action, pane: number): Promise<Response> {
  const magmux = magmuxURL();
  if (!magmux) return json({ by: "serve.ts", error: "magmux has not bound yet" }, 503);

  let token = viewToken;
  if (act.cred === "session") {
    const got = sessionToken();
    if ("error" in got) return json({ by: "serve.ts", error: got.error }, 503);
    token = got.token;
  }

  if (act.resolve === "opened") {
    const found = await openedPane(magmux, token);
    if ("error" in found) return json({ by: "serve.ts", error: found.error }, 409);
    pane = found.pane;
  }

  const body = act.body ? JSON.stringify(act.body(pane)) : undefined;
  const headers: Record<string, string> = { Authorization: `Bearer ${token}` };
  if (body !== undefined) headers["Content-Type"] = "application/json";

  const started = Date.now();
  let res: Response;
  try {
    res = await fetch(magmux + act.path, { method: act.method, headers, body });
  } catch (err: any) {
    return json(
      {
        by: "serve.ts",
        error: `the request to magmux never completed: ${err?.message ?? err}`,
        upstream: { method: act.method, path: act.path, cred: act.cred, body: body ?? null },
        ms: Date.now() - started,
      },
      502,
    );
  }
  const ms = Date.now() - started;
  const text = await res.text();

  // magmux's status, unchanged, including the 403s. The envelope repeats it so
  // a panel that only reads the body still sees the truth.
  return new Response(
    JSON.stringify({
      action: act.name,
      request: { method: act.method, path: act.path, cred: act.cred, body: body ?? null },
      status: res.status,
      text,
      ms,
    }),
    {
      status: res.status,
      headers: {
        "Content-Type": "application/json",
        "Cache-Control": "no-store",
        // So a reader can tell whose answer this is without parsing it.
        "X-Magmux-Upstream": String(res.status),
      },
    },
  );
}

let server: ReturnType<typeof Bun.serve>;
try {
  server = Bun.serve({
    hostname: args.host,
    port: args.port,
    async fetch(req) {
      const url = new URL(req.url);
      if (url.pathname === "/" || url.pathname === "/index.html") {
        return new Response(indexHTML, {
          headers: { "Content-Type": "text/html; charset=utf-8", "Cache-Control": "no-store" },
        });
      }
      if (url.pathname === "/app.js") {
        return new Response(appJS, {
          headers: { "Content-Type": "text/javascript; charset=utf-8", "Cache-Control": "no-store" },
        });
      }
      if (url.pathname === configPath) {
        if (!sameOrigin(req, server.port)) {
          return json({ error: "this endpoint is only for the page served from this origin" }, 403);
        }
        const magmux = magmuxURL();
        if (!magmux) {
          // A legible error beats a blank page. A page that silently got no URL
          // would fall back to its own origin, get a 404 on /v1/ws, and look
          // like an auth failure — sending whoever is debugging it to the token.
          return json({ error: "magmux has not bound yet" }, 503);
        }
        // `driver` is how the page learns the endpoint's path: it travels in a
        // same-origin response rather than in the URL, so the one path that can
        // type into a shell is never in an address bar, a screenshot or a shell
        // history. It is null when RC_DEMO_WEB_DRIVER=0, and the page then has
        // no panel to show.
        return json({ magmux, viewToken, pane: args.pane, driver: driverPath || null });
      }

      // ── the driver endpoint ────────────────────────────────────────────
      // GET  → the allow-list itself, so the panel is BUILT from it and cannot
      //        show a button this server would refuse.
      // POST → run one named action. See the header comment: this is where a
      //        read-only page borrows the ability to type.
      if (driverPath && url.pathname === driverPath) {
        if (!sameOrigin(req, server.port)) {
          return json({ by: "serve.ts", error: "this endpoint is only for the page served from this origin" }, 403);
        }
        if (req.method === "GET") {
          return json({
            actions: ACTIONS.map((a) => ({
              name: a.name,
              item: a.item,
              group: a.group ?? null,
              title: a.title,
              how: a.how,
              method: a.method,
              path: a.path,
              cred: a.cred,
              pane: a.pane,
              look: a.look,
            })),
          });
        }
        if (req.method !== "POST") {
          return json({ by: "serve.ts", error: `${req.method} is not how this endpoint is asked` }, 405);
        }
        let asked: any = null;
        try {
          asked = await req.json();
        } catch {
          return json({ by: "serve.ts", error: "the body is not JSON" }, 400);
        }
        const act = ACTIONS.find((a) => a.name === asked?.action);
        if (!act) {
          // Refused by THIS SERVER, and it says so. An allow-list that failed
          // open — or that forwarded an unknown action to magmux to be refused
          // there — would be a general proxy wearing a list's clothes.
          return json(
            {
              by: "serve.ts",
              error: `"${String(asked?.action).slice(0, 60)}" is not one of this demo's driver actions`,
              allowed: ACTIONS.map((a) => a.name),
            },
            400,
          );
        }
        // The ONE value a request may carry, and it is an integer or nothing.
        let pane = -1;
        if (act.pane === "target") {
          const p = asked?.pane;
          if (!Number.isInteger(p) || p < 0 || p > 9999) {
            return json({ by: "serve.ts", error: `action "${act.name}" needs an integer pane id` }, 400);
          }
          pane = p;
        }
        return await runAction(act, pane);
      }

      return new Response("not found\n", { status: 404 });
    },
  });
} catch (err: any) {
  abort(`cannot bind ${args.host}:${args.port}: ${err?.message ?? err}`);
}

/**
 * Exactly one line on stdout, and it is the readiness signal.
 *
 * `url` is what the launcher opens: it carries the config path in `?c=`, which
 * is how the page learns where its own config lives. Everything else this
 * script has to say goes to stderr, so the contract is a single parse and not a
 * search.
 */
const openURL = `http://${args.host}:${server.port}/?c=${encodeURIComponent(configPath)}`;
process.stdout.write(
  `ready ${JSON.stringify({
    port: server.port,
    url: openURL,
    origin: `http://${args.host}:${server.port}`,
    configPath,
    // Absent — not empty — when RC_DEMO_WEB_DRIVER=0, so a launcher or a check
    // can tell "off" from "on with a path I failed to parse".
    ...(driverPath ? { driverPath } : {}),
    viewTokenFile: path.resolve(args.tokenFile),
    urlFile: URL_FILE,
  })}\n`,
);
if (!driverEnabled) {
  process.stderr.write("serve: RC_DEMO_WEB_DRIVER=0 — no driver endpoint; the page is purely read-only\n");
}

/**
 * The ppid watchdog.
 *
 * An orphaned static server still holding the view token is the leak that
 * matters — the demo's other two processes are a tmux session and a magmux,
 * both of which announce themselves, and this one does not. When the launcher
 * dies (including `kill -9`, which runs no trap), init reparents this process
 * and `process.ppid` becomes 1. That is the signal to go, and to take the token
 * with us.
 */
const watchdog = setInterval(() => {
  if (process.ppid === 1) {
    try {
      fs.rmSync(args.tokenFile, { force: true });
    } catch {}
    process.exit(0);
  }
}, 1_000);
// Do not let the watchdog alone keep bun alive; the server already does.
(watchdog as any).unref?.();

for (const sig of ["SIGINT", "SIGTERM"] as const) {
  process.on(sig, () => {
    server.stop(true);
    process.exit(0);
  });
}
