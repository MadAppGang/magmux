/**
 * The browser half of the remote-control demo: a DOM `Painter` on `client.ts`,
 * plus the driver panel beside it.
 *
 * `serve.ts` bundles this file with Bun.build, so the imports below resolve to
 * the SAME `../client.ts` and `../frame.ts` the terminal client uses. That is
 * the demo's claim made structural rather than asserted: one hub, one decoder,
 * two surfaces. Nothing in this file knows what a WebSocket frame message looks
 * like — except the ONE place that prints one verbatim as evidence, which opens
 * its own read-only connection to do it.
 *
 * It is a PAINTER, not a terminal emulator. Every row arrives decoded, as spans
 * with resolved colours and attributes; the only thing here that reads the wire
 * is the colour CLASSIFICATION, which it turns into CSS exactly as `cli.ts`
 * turns it into SGR. `Painter.row()` takes a decoded row and cell columns,
 * never a string, and that rule is what stops this page becoming a second
 * terminal emulator.
 *
 * THE DRIVER PANEL HOLDS NO SESSION TOKEN. Its buttons POST an action NAME to
 * `serve.ts`, which holds both credentials and performs the action, and what is
 * printed here is magmux's own status, body and latency — including the 403s.
 * See serve.ts's header for the escalation that buys, and for
 * `RC_DEMO_WEB_DRIVER=0`, which removes it.
 */
import { RemoteClient, type Painter, type Status } from "../client.ts";
import {
  ATTR_BOLD,
  ATTR_DIM,
  ATTR_INVIS,
  ATTR_ITALIC,
  ATTR_OVERLINE,
  ATTR_REVERSE,
  ATTR_STRIKE,
  ATTR_UNDERLINE,
  classifyColour,
  rowSpans,
  type CellCols,
  type Cursor,
  type Line,
  type Span,
} from "../frame.ts";

// ── the xterm-256 palette, computed ─────────────────────────────────────────

/**
 * All 256 colours, built rather than shipped as a table.
 *
 * 232 of the 256 are pure arithmetic — a 6x6x6 cube and a 24-step grey ramp —
 * and a literal table of them is 232 chances to fat-finger a digit that nobody
 * would ever notice. The first 16 are the terminal's OWN choice and are not
 * derivable from anything, so they are generated from the bit pattern with
 * xterm's two documented departures from it spelled out.
 */
function buildPalette(): string[] {
  const out: string[] = [];
  const css = (r: number, g: number, b: number) => `rgb(${r},${g},${b})`;

  for (let i = 0; i < 16; i++) {
    const bright = i >= 8;
    const n = i & 7;
    const v = bright ? 255 : 205;
    let r = n & 1 ? v : 0;
    let g = n & 2 ? v : 0;
    let b = n & 4 ? v : 0;
    // xterm's two departures from the bit pattern, both for legibility on a
    // dark background: blue is lifted off the background, and white is not
    // "every channel at full".
    if (n === 4) {
      r = bright ? 92 : 0;
      g = r;
      b = bright ? 255 : 238;
    }
    if (n === 7) r = g = b = bright ? 255 : 229;
    if (i === 8) r = g = b = 127;
    out.push(css(r, g, b));
  }

  // 16..231: the 6x6x6 cube. The levels are not evenly spaced — 0 then
  // 95,135,175,215,255 — which is why the first step is special-cased.
  const level = (v: number) => (v === 0 ? 0 : 55 + 40 * v);
  for (let n = 0; n < 216; n++) {
    out.push(css(level(Math.floor(n / 36)), level(Math.floor(n / 6) % 6), level(n % 6)));
  }

  // 232..255: the grey ramp, 8 to 238 in steps of 10.
  for (let n = 0; n < 24; n++) {
    const v = 8 + n * 10;
    out.push(css(v, v, v));
  }
  return out;
}

const PALETTE = buildPalette();

/**
 * The screen's own foreground and background, used for the `default` colour.
 *
 * `--term-fg`, NOT `--fg`: the page's foreground flips with
 * prefers-color-scheme and the SCREEN's background deliberately does not, so a
 * default-coloured cell painted in `--fg` comes out dark on dark the moment
 * somebody's system is in light mode.
 */
const DEFAULT_FG = "var(--term-fg)";
const DEFAULT_BG = "transparent";

function cssColour(c: number, fallback: string): string {
  const k = classifyColour(c);
  if (k.kind === "rgb") return `rgb(${k.r},${k.g},${k.b})`;
  if (k.kind === "indexed") return PALETTE[k.index] ?? fallback;
  return fallback;
}

/**
 * One span's style as CSS.
 *
 * `reverse` is resolved HERE, by swapping the two resolved colours, rather than
 * by a CSS filter: a filter would also invert the page's own default fore and
 * background, and a reversed default-on-default cell — which is what a
 * selection or a status bar is made of — would come out invisible.
 */
function spanStyle(span: Span): string {
  let fg = cssColour(span.fg, DEFAULT_FG);
  let bg = cssColour(span.bg, DEFAULT_BG);
  const a = span.attr;
  if (a & ATTR_REVERSE) {
    const swapFg = span.bg < 0 ? "var(--term-bg)" : bg;
    const swapBg = span.fg < 0 ? "var(--term-fg)" : fg;
    fg = swapFg;
    bg = swapBg;
  }
  const bits: string[] = [`color:${fg}`];
  if (bg !== DEFAULT_BG) bits.push(`background:${bg}`);
  if (a & ATTR_BOLD) bits.push("font-weight:700");
  if (a & ATTR_DIM) bits.push("opacity:.6");
  if (a & ATTR_ITALIC) bits.push("font-style:italic");
  // ATTR_BLINK is deliberately not reproduced: a mirror that blinks is a
  // distraction rather than fidelity, and the bit is still in the decoded span
  // for anything that wants it.
  if (a & ATTR_INVIS) bits.push("visibility:hidden");
  const lines: string[] = [];
  if (a & ATTR_UNDERLINE) lines.push("underline");
  if (a & ATTR_STRIKE) lines.push("line-through");
  if (a & ATTR_OVERLINE) lines.push("overline");
  if (lines.length) bits.push(`text-decoration:${lines.join(" ")}`);
  return bits.join(";");
}

// ── the painter ─────────────────────────────────────────────────────────────

const $ = (id: string) => document.getElementById(id)!;

/**
 * The size band. Below the floor a mirror is unreadable and above the ceiling
 * an 80-column pane on a 5K display is a novelty poster, so the terminal fills
 * the width it is given only within a range somebody would actually read.
 */
const MIN_FONT_PX = 7;
const MAX_FONT_PX = 30;
/** Must match --lh in index.html; one number, stated in both places it is used. */
const LINE_HEIGHT = 1.25;
/** #screen's own padding, from the stylesheet: 8px top/bottom, 10px sides. */
const SCREEN_PAD_X = 20;
const SCREEN_PAD_Y = 16;

class DOMPainter implements Painter {
  private rows: HTMLDivElement[] = [];
  private cols = 0;
  private cellW = 8;
  private cellH = 17;
  private curVisible = false;
  /** True when the grid is wider than the room, at the smallest readable size. */
  private overflowing = false;
  private readonly screenEl = $("screen");
  private readonly termEl = $("term");
  private readonly titleEl = $("termtitle");
  private readonly curEl = this.screenEl.querySelector(".cur") as HTMLDivElement;
  private readonly measureEl = $("measure");
  private readonly statusEl = $("status");
  private readonly dotEl = $("dot");
  private readonly stateEl = $("state");
  private readonly detailEl = $("detail");
  private readonly reasonEl = $("reason");
  private readonly reconnectEl = $("reconnect") as HTMLButtonElement;
  private readonly panesEl = $("panes") as HTMLSelectElement;
  /** What the picker currently shows, so it is rebuilt only when it has to be. */
  private paneKey = "";
  /** The last cursor the wire reported, so a resize can redraw it in place. */
  private lastCur: Cursor = { y: 0, x: 0, vis: false };

  /**
   * Build the row elements for a geometry, and fit them to the window.
   *
   * IDEMPOTENT BY CONTRACT: `client.ts` calls this from the watch reply AND
   * from every frame header, because geometry is not fixed for the life of a
   * watch — dragging the window SIGWINCHes magmux and reflows the pane. The
   * early return is what makes the frame path free.
   */
  size(rows: number, cols: number) {
    if (this.rows.length === rows && this.cols === cols) return;
    this.cols = cols;
    for (const r of this.rows) r.remove();
    this.rows = [];
    for (let y = 0; y < rows; y++) {
      const el = document.createElement("div");
      el.className = "row";
      el.textContent = " ";
      this.screenEl.appendChild(el);
      this.rows.push(el);
    }
    this.fit();
  }

  /**
   * Compute the font size that makes the pane FILL the stage, and hand it to
   * CSS as one variable.
   *
   * BOTH DIMENSIONS, and the LIMITING one wins — `Math.min` of the width fit
   * and the height fit. Either one alone is wrong in a way that is easy to
   * mistake for a bug in the mirror: fitting to width only makes a 36-row pane
   * in a short window overflow and scroll, and fitting to height only leaves a
   * 200-column pane in a letterbox strip with the page's own background either
   * side of it. The grid is then sized to the cells, so the terminal's frame is
   * the pane's edge rather than wherever the text happened to stop.
   *
   * This is a RESIZE concern, not a frame concern: it runs when the geometry
   * changes and when the window does — including when the driver panel is
   * hidden, which changes the stage and nothing else — and never per frame. The
   * measurement is of the font the browser actually chose: a monospace advance
   * is not a fixed fraction of the font size across fonts, and a `ch`
   * assumption that is a percent out puts the cursor a column and a half to the
   * right by column 80.
   */
  fit() {
    if (!this.cols || !this.rows.length) return;
    const stage = $("stage");
    const pad = 36; // #stage's own padding, both sides
    const chrome = this.termEl.getBoundingClientRect().height - this.screenEl.getBoundingClientRect().height;
    const availW = Math.max(120, stage.clientWidth - pad - 2 - SCREEN_PAD_X);
    const availH = Math.max(60, stage.clientHeight - pad - 2 - SCREEN_PAD_Y - Math.max(0, chrome));

    // The advance width as a FRACTION of the font size, measured once per fit
    // at a large probe size so the rounding is a rounding of one pixel in a
    // hundred rather than one in fourteen.
    this.measureEl.style.fontSize = "100px";
    const ratio = this.measureEl.getBoundingClientRect().width / 2000 || 0.6;
    this.measureEl.style.fontSize = "";

    let fs = Math.min(availW / (this.cols * ratio), availH / (this.rows.length * LINE_HEIGHT));
    fs = Math.max(MIN_FONT_PX, Math.min(MAX_FONT_PX, Math.floor(fs * 2) / 2));
    document.documentElement.style.setProperty("--fs", `${fs}px`);

    // Re-measure at the size actually applied and step down if the real advance
    // rounded up past the room available. Three steps is enough for a half-pixel
    // disagreement and bounds the work; overflow is hidden either way, so the
    // worst case is a clipped last column rather than a broken layout.
    for (let i = 0; i < 3; i++) {
      this.cellW = this.measureEl.getBoundingClientRect().width / 20;
      if (this.cellW * this.cols <= availW || fs <= MIN_FONT_PX) break;
      fs = Math.max(MIN_FONT_PX, fs - 0.5);
      document.documentElement.style.setProperty("--fs", `${fs}px`);
    }
    this.cellH = fs * LINE_HEIGHT;
    // A pane wider than the smallest readable font can fit scrolls, and the
    // termbar says so. Shrinking past the floor would be unreadable and
    // clipping in silence would make the mirror disagree with the pane while
    // looking perfectly healthy — the one failure mode this demo forbids.
    this.overflowing = this.cellW * this.cols > availW + 0.5;

    // Size the frame to the GRID, so the terminal's edge is the pane's edge and
    // not wherever the last row's text happened to stop.
    this.screenEl.style.width = `${Math.ceil(this.cellW * this.cols) + SCREEN_PAD_X}px`;
    this.screenEl.style.height = `${Math.ceil(this.cellH * this.rows.length) + SCREEN_PAD_Y}px`;
    this.cursor(this.lastCur);
  }

  clear() {
    for (const r of this.rows) r.replaceChildren(document.createTextNode(" "));
  }

  row(y: number, line: Line, cells: CellCols) {
    const el = this.rows[y];
    if (!el) return;
    const frag = document.createDocumentFragment();
    for (const span of rowSpans(line, cells, this.cols)) {
      const s = document.createElement("span");
      // textContent, never innerHTML: a pane is a shell, and a shell can print
      // anything at all. A mirror that interpreted its subject's output as
      // markup would be a cross-site scripting vector driven by whatever the
      // session happens to `cat`.
      s.textContent = span.text;
      const style = spanStyle(span);
      if (style) s.setAttribute("style", style);
      frag.appendChild(s);
    }
    el.replaceChildren(frag);
  }

  /**
   * The cursor, in the cell the frame header names.
   *
   * `vis` is DECTCEM, decoded upstream: a hidden cursor is NOT drawn, which is
   * the faithful thing to do, and the status bar then says `cursor hidden` so
   * an absent caret cannot be read as a mirror that has stopped following.
   */
  cursor(c: Cursor) {
    this.lastCur = c;
    this.curVisible = c.vis && c.y < this.rows.length;
    if (!this.curVisible) {
      this.curEl.style.display = "none";
      return;
    }
    // offsetLeft/offsetTop are relative to #screen, which is position:relative
    // and is the .cur element's containing block. Client rects would need the
    // page's own scroll subtracted back out and drift the moment it scrolls.
    const row = this.rows[c.y];
    this.curEl.style.display = "block";
    this.curEl.style.width = `${this.cellW}px`;
    this.curEl.style.height = `${this.cellH}px`;
    this.curEl.style.left = `${row.offsetLeft + c.x * this.cellW}px`;
    this.curEl.style.top = `${row.offsetTop}px`;
  }

  status(s: Status) {
    this.statusEl.className = s.state;
    this.dotEl.textContent = s.state === "live" ? "●" : s.state === "stale" ? "◐" : s.state === "connecting" ? "○" : "✗";
    this.stateEl.textContent = s.state;

    const bits = [`pane ${s.pane}`];
    if (s.cols && s.rows) bits.push(`${s.cols}×${s.rows}`);
    bits.push(`seq ${s.seq}`);
    bits.push(`${s.frames} frames`);
    if (s.fps) bits.push(`${s.fps} fps`);
    bits.push(s.ageMs < 0 ? "no frame yet" : `last frame ${(s.ageMs / 1000).toFixed(1)}s ago`);
    if (s.alt) bits.push("alt screen");
    if (!this.curVisible && s.state === "live") bits.push("cursor hidden");
    if (s.scrolled) bits.push("the local viewer has scrolled back");
    this.detailEl.textContent = bits.join(" · ");
    const scrollNote = this.overflowing ? ` · at the minimum size — scroll for all ${s.cols} columns` : "";
    this.titleEl.textContent =
      s.cols && s.rows
        ? `pane ${s.pane} · ${s.cols}×${s.rows}${s.alt ? " · alt screen" : ""}${scrollNote}`
        : `pane ${s.pane}`;

    // Dim the picture the moment it stops being current. A stale screen that
    // looks exactly like a live one is the single failure this whole status
    // bar exists to prevent.
    this.screenEl.classList.toggle("stale", s.state === "stale");
    this.screenEl.classList.toggle("dead", s.state === "dead" || s.state === "ended");
    this.reasonEl.textContent = s.state === "live" || s.state === "connecting" ? "" : s.reason;
    this.pickerFrom(s);
  }

  /**
   * The pane picker, rebuilt only when the pane list or the selection changes.
   *
   * The control panel is excluded: it is magmux's own chrome rather than a
   * session, it never changes, and a viewer who picked it would conclude the
   * mirror had frozen. Rebuilding on every status tick would also close the
   * dropdown under the mouse of anyone using it, twice a second.
   */
  private pickerFrom(s: Status) {
    const list = s.panes.filter((p) => p.state !== "panel");
    const key = `${s.pane}|${list.map((p) => `${p.pane}:${p.label}`).join(",")}`;
    if (key === this.paneKey) return;
    this.paneKey = key;
    this.panesEl.replaceChildren(
      ...list.map((p) => {
        const o = document.createElement("option");
        o.value = String(p.pane);
        o.textContent = p.label ? `pane ${p.pane} · ${p.label}` : `pane ${p.pane}`;
        o.selected = p.pane === s.pane;
        return o;
      }),
    );
    this.panesEl.disabled = list.length < 2;
  }

  end(reason: string, ok: boolean) {
    this.reasonEl.textContent = reason;
    this.screenEl.classList.add("dead");
    this.statusEl.className = ok ? "ended" : "dead";
    this.dotEl.textContent = "✗";
    this.stateEl.textContent = ok ? "ended" : "dead";
    this.curEl.style.display = "none";
    // A picker on a dead connection can only produce a refusal, so it is
    // disabled rather than left looking like something that still works.
    // `paneKey` is cleared with it: Reconnect builds a NEW client, and a
    // cached key would leave the picker disabled for the rest of the page's
    // life against a connection that was working again.
    this.panesEl.disabled = true;
    this.paneKey = "";
    // A button, and NO automatic retry. A silent retry loop behind a picture
    // that never changes is precisely the still-picture failure this demo is
    // required to make impossible.
    this.reconnectEl.classList.add("show");
  }
}

// ── the driver panel ────────────────────────────────────────────────────────

/** One entry of serve.ts's allow-list, as it serves it. */
interface ActionSpec {
  name: string;
  item: string;
  group: string | null;
  title: string;
  how: string;
  method: string;
  path: string;
  cred: "session" | "view";
  pane: "target" | "none";
  look: string;
}

/** What `serve.ts` answers a POST with. */
interface ActionResult {
  action?: string;
  request?: { method: string; path: string; cred: string; body: string | null };
  status?: number;
  text?: string;
  ms?: number;
  /** Present ONLY on a refusal serve.ts decided itself. Never magmux's. */
  by?: string;
  error?: string;
  allowed?: string[];
}

/** One line of JSON, short enough for a panel. The same rule the driver uses. */
function brief(raw: string, max = 240): string {
  const s = raw.replace(/\s*\n\s*/g, " ").trim();
  return s.length <= max ? s : `${s.slice(0, max)} …(${s.length} chars)`;
}

/**
 * The panel: serve.ts's allow-list, rendered, with the request and the response
 * printed in the shape the driver prints them (`▶ POST …` / `◀ HTTP 200 …`), so
 * the two drivers show identical evidence for the same action.
 *
 * It never invents a response. Every `◀` line below is built from what came
 * back: magmux's status and body when magmux answered, and an explicitly
 * labelled `serve.ts refused` line when this demo's own server refused before
 * magmux was reached.
 */
class DriverPanel {
  private readonly panelEl = $("driver");
  private readonly actionsEl = $("actions");
  private readonly logEl = $("log");
  private busy = false;

  constructor(
    private readonly driverPath: string,
    /** The pane this page is watching right now — the ONE value a request carries. */
    private readonly pane: () => number,
    /** Item 13: the raw frame, done in the browser with the view token. */
    private readonly rawFrame: (pane: number) => Promise<string>,
  ) {}

  async load(): Promise<void> {
    const res = await fetch(this.driverPath, { cache: "no-store" });
    if (!res.ok) throw new Error(`the driver endpoint answered ${res.status}`);
    const list = ((await res.json())?.actions ?? []) as ActionSpec[];
    if (!list.length) throw new Error("the driver endpoint served no actions");

    for (const a of list) {
      if (a.group) this.actionsEl.appendChild(this.groupEl(a.group));
      this.actionsEl.appendChild(this.buttonEl(a, () => this.run(a)));
    }
    // The one item that needs NO escalation, so it must not have one: the raw
    // frame is read off this page's own read-only connection.
    this.actionsEl.appendChild(this.groupEl("THE WIRE"));
    this.actionsEl.appendChild(
      this.buttonEl(
        {
          name: "raw-frame",
          item: "13",
          group: null,
          title: "print the next raw frame",
          how: "this page's OWN view-token connection — no server, no session token",
          method: "WS",
          path: "/v1/ws",
          cred: "view",
          pane: "target",
          look: "that is byte for byte what this page and the terminal mirror decode",
        },
        () => this.runRaw(),
      ),
    );
    this.panelEl.classList.remove("off");
  }

  private groupEl(name: string): HTMLElement {
    const g = document.createElement("div");
    g.className = "group";
    g.textContent = name;
    return g;
  }

  private buttonEl(a: ActionSpec, onClick: () => Promise<void>): HTMLButtonElement {
    const b = document.createElement("button");
    b.type = "button";
    b.className = `act${a.cred === "view" ? " view" : ""}${a.method === "WS" ? " local" : ""}`;
    b.dataset.action = a.name;
    const k = document.createElement("span");
    k.className = "k";
    k.textContent = a.item;
    const t = document.createTextNode(a.title);
    const how = document.createElement("span");
    how.className = "how";
    how.textContent = a.how;
    b.append(k, t, how);
    b.addEventListener("click", () => {
      if (this.busy) return;
      this.busy = true;
      b.disabled = true;
      void onClick().finally(() => {
        this.busy = false;
        b.disabled = false;
      });
    });
    return b;
  }

  /** One log entry: the request line, the response line, and where to look. */
  private entry(): { req: (s: string) => void; res: (s: string, cls: string) => void; look: (s: string) => void } {
    const idle = this.logEl.querySelector(".idle");
    if (idle) idle.remove();
    const div = document.createElement("div");
    div.className = "entry";
    const req = document.createElement("div");
    req.className = "req";
    const res = document.createElement("div");
    res.className = "res";
    const look = document.createElement("div");
    look.className = "look";
    div.append(req, res, look);
    this.logEl.appendChild(div);
    // Newest last, and scrolled to: this is a transcript, exactly as the
    // driver pane is a scrolling pane rather than a dashboard.
    while (this.logEl.children.length > 40) this.logEl.firstChild!.remove();
    const scroll = () => (this.logEl.scrollTop = this.logEl.scrollHeight);
    scroll();
    return {
      req: (s) => {
        req.textContent = s;
        scroll();
      },
      res: (s, cls) => {
        res.className = `res ${cls}`;
        res.textContent = s;
        scroll();
      },
      look: (s) => {
        look.textContent = s ? `↳ ${s}` : "";
        scroll();
      },
    };
  }

  private async run(a: ActionSpec): Promise<void> {
    const e = this.entry();
    const pane = this.pane();
    e.req(`▶ ${a.method} … · ${a.title}`);
    e.res("… waiting for magmux", "ms");

    let res: Response;
    let body: ActionResult;
    const started = Date.now();
    try {
      res = await fetch(this.driverPath, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        // The action NAME, and at most the pane this page is watching. Every
        // argument magmux sees is written in serve.ts.
        body: JSON.stringify(a.pane === "target" ? { action: a.name, pane } : { action: a.name }),
      });
      body = (await res.json()) as ActionResult;
    } catch (err: any) {
      e.req(`▶ POST ${this.driverPath.slice(0, 12)}… · ${a.name}`);
      e.res(`◀ the request to this demo's own server never completed: ${err?.message ?? err}`, "bad mine");
      return;
    }

    if (body?.by === "serve.ts") {
      // THIS SERVER's refusal, labelled as such. It is not magmux's answer and
      // must never be printed as though it were.
      e.req(`▶ POST ${this.driverPath.slice(0, 12)}…  {"action":"${a.name}"}  · to serve.ts`);
      e.res(
        `◀ serve.ts refused, HTTP ${res.status}  ${brief(JSON.stringify(body))}  ${Date.now() - started}ms`,
        "mine",
      );
      e.look("this demo's own server, before magmux was reached — not a magmux refusal");
      return;
    }

    const rq = body.request;
    const cred = rq?.cred === "view" ? "the VIEW token" : "the session token";
    e.req(`▶ ${rq?.method ?? a.method} ${rq?.path ?? a.path}  ${rq?.body ? brief(rq.body, 140) : ""} · as ${cred}`);
    e.res(`◀ HTTP ${body.status ?? res.status}  ${brief(body.text ?? "")}  ${body.ms ?? Date.now() - started}ms`, res.ok ? "" : "bad");
    e.look(a.look);
  }

  /** Item 13, in the browser, over this page's own read-only credential. */
  private async runRaw(): Promise<void> {
    const e = this.entry();
    const pane = this.pane();
    e.req(`▶ WS /v1/ws  watch pane ${pane} · as the VIEW token, from this page`);
    e.res("… waiting for a frame (an idle pane legitimately produces none)", "ms");
    const started = Date.now();
    try {
      const raw = await this.rawFrame(pane);
      const ev = JSON.parse(raw);
      e.res(
        `◀ ${ev.key ? "KEYFRAME" : "delta"} · pane ${ev.pane} · seq ${ev.seq} · ${ev.cols}×${ev.rows} · ` +
          `${(ev.lines ?? []).length} rows · cursor ${ev.cur?.y},${ev.cur?.x} · ${raw.length} bytes  ` +
          `${Date.now() - started}ms\n${brief(raw, 420)}`,
        "",
      );
      e.look("each line is {y, t, r}; each run is [col, len, fg, bg, attr] — the shared decoder's input");
    } catch (err: any) {
      e.res(`◀ ${err?.message ?? err}`, "bad");
    }
  }
}

/**
 * Open a SECOND read-only connection, take the next frame for a pane, and close.
 *
 * It is a separate connection on purpose: the page's own client is a decoder
 * and its `Painter` contract takes decoded rows, never raw JSON. Putting a raw
 * hook into the shared core to serve one panel item would be exactly the
 * erosion the `Painter.row()` rule exists to prevent.
 */
function nextRawFrame(url: string, token: string, pane: number, withinMs = 6_000): Promise<string> {
  return new Promise((resolve, reject) => {
    const ws = new WebSocket(`${url.replace(/^http/, "ws")}/v1/ws`, ["magmux.v1", `magmux.auth.${token}`]);
    const timer = setTimeout(() => {
      ws.close();
      reject(new Error(`no frame arrived within ${withinMs / 1000}s — the pane is idle, which is legitimate`));
    }, withinMs);
    const done = (fn: () => void) => {
      clearTimeout(timer);
      try {
        ws.close();
      } catch {}
      fn();
    };
    ws.addEventListener("open", () => ws.send(JSON.stringify({ id: 1, op: "watch", args: { pane, mode: "frames", fps: 15 } })));
    ws.addEventListener("message", (m: MessageEvent) => {
      if (typeof m.data !== "string") return;
      let ev: any;
      try {
        ev = JSON.parse(m.data);
      } catch {
        return;
      }
      if (ev.type === "frame" && ev.pane === pane) done(() => resolve(m.data as string));
    });
    ws.addEventListener("close", (ev: CloseEvent) =>
      done(() => reject(new Error(`the connection closed before a frame arrived (code ${ev.code})`))),
    );
  });
}

// ── entry point ─────────────────────────────────────────────────────────────

interface Config {
  magmux: string;
  viewToken: string;
  pane: number | null;
  /** The driver endpoint's per-run path, or null under RC_DEMO_WEB_DRIVER=0. */
  driver: string | null;
}

/**
 * Where this page's config lives.
 *
 * `serve.ts` mints an unguessable per-run path and puts it in the URL the
 * launcher opens, so the page learns it from its own location and it appears
 * nowhere else. The TOKEN is still never in a URL — this is a path to an
 * endpoint that will only answer a same-origin request, not the credential
 * itself. The DRIVER's path is not in the URL either: it rides in the config
 * response, so the one path that can type into a shell never reaches an address
 * bar, a screenshot or a shell history.
 */
function configPath(): string {
  const c = new URLSearchParams(location.search).get("c");
  if (!c) {
    throw new Error(
      "this page was opened without its config path — open the URL `task demo:rc` printed, not just the host and port",
    );
  }
  return c;
}

async function loadConfig(): Promise<Config> {
  // Same origin, so no token travels in a URL, a fragment or a paste buffer.
  // `serve.ts` sets no Access-Control-Allow-Origin on this response, so another
  // origin may make the request and cannot read the answer; it also refuses any
  // request that does not carry Sec-Fetch-Site: same-origin, which the browser
  // sets on this fetch and which a curl from another shell does not.
  const res = await fetch(configPath(), { cache: "no-store" });
  if (!res.ok) {
    let why = "";
    try {
      why = (await res.json())?.error ?? "";
    } catch {}
    if (res.status === 503) {
      throw new Error(`${why || "magmux has not bound yet"} — reconnect once it has`);
    }
    throw new Error(`the demo's config endpoint returned ${res.status}${why ? `: ${why}` : ""}`);
  }
  return (await res.json()) as Config;
}

const painter = new DOMPainter();
let running = false;
/** Built once, on the first successful connect; a Reconnect must not duplicate it. */
let panel: DriverPanel | null = null;

// The fit is a RESIZE concern: once per window change, never per frame. rAF
// coalesces a drag into one measurement per painted frame of the browser's own.
let pending = false;
function refit() {
  if (pending) return;
  pending = true;
  requestAnimationFrame(() => {
    pending = false;
    painter.fit();
  });
}
addEventListener("resize", refit);

/**
 * FULL WIDTH: the terminal takes the page, the driver panel and the footer go.
 *
 * It is a class on <body> and nothing else, so the terminal is never rebuilt,
 * never reconnected and never loses a frame — the stage simply becomes wider
 * and `fit()` measures what it actually got. The refit goes through the same
 * rAF path a window resize does, one measurement after the layout has settled.
 *
 * A BUTTON AND A KEY, because the audiences differ: `f` is for whoever is
 * presenting, and the control is how somebody who has never seen this page
 * learns the option exists at all. `Escape` only ever LEAVES, which is the one
 * thing an escape key may mean.
 *
 * The status bar is deliberately not hidden with the rest: it is the only
 * surface that can say the picture has stopped being live, and a full-screen
 * mirror with no way to say so is exactly the still picture this demo forbids.
 */
const fullEl = $("full") as HTMLButtonElement;
function setFull(on: boolean) {
  if (document.body.classList.contains("full") === on) return;
  document.body.classList.toggle("full", on);
  fullEl.setAttribute("aria-pressed", String(on));
  fullEl.textContent = on ? "⤢ exit full width" : "⤢ full width";
  refit();
}
fullEl.addEventListener("click", () => setFull(!document.body.classList.contains("full")));
addEventListener("keydown", (e: KeyboardEvent) => {
  // Never swallow a key a control is using — the pane picker is a <select>, and
  // `f` inside an open one is type-ahead — and never one carrying a modifier,
  // which belongs to the browser.
  if (e.metaKey || e.ctrlKey || e.altKey) return;
  const t = e.target as HTMLElement | null;
  if (t && (t.tagName === "SELECT" || t.tagName === "INPUT" || t.tagName === "TEXTAREA")) return;
  if (e.key === "f" || e.key === "F") {
    e.preventDefault();
    setFull(!document.body.classList.contains("full"));
  } else if (e.key === "Escape") {
    setFull(false);
  }
});

async function connect() {
  if (running) return;
  running = true;
  ($("reconnect") as HTMLButtonElement).classList.remove("show");
  $("reason").textContent = "";
  try {
    const cfg = await loadConfig();
    const client = new RemoteClient(
      {
        url: cfg.magmux,
        token: cfg.viewToken,
        pane: cfg.pane ?? null,
        fps: 15,
        client: "magmux-rc-demo/web",
        verbose: new URLSearchParams(location.search).has("verbose"),
        log: (m) => console.log("[magmux]", m),
      },
      painter,
    );
    // Leave cleanly on navigation so magmux stops framing a pane nobody
    // watches, rather than waiting for the socket to time out.
    addEventListener("pagehide", () => client.quit("the page was closed"), { once: true });
    // The picker is the page retargeting ITSELF, through the same two public
    // ops any client has — `unwatch` then `watch` on its own connection. There
    // is no op that points somebody else's stream at a different pane, and
    // there should not be.
    const picker = $("panes") as HTMLSelectElement;
    picker.onchange = () => {
      const why = client.retarget(Number(picker.value));
      if (why) $("reason").textContent = why;
    };

    // The panel, only when serve.ts minted an endpoint for it. Under
    // RC_DEMO_WEB_DRIVER=0 there is no path, no panel, and no escalation note.
    if (cfg.driver && !panel) {
      panel = new DriverPanel(
        cfg.driver,
        () => client.watchingPane() ?? 0,
        (pane) => nextRawFrame(cfg.magmux, cfg.viewToken, pane),
      );
      try {
        await panel.load();
      } catch (err: any) {
        panel = null;
        console.warn("[magmux] the driver panel did not load:", err?.message ?? err);
      }
    }
    if (!cfg.driver) $("escalation").classList.add("off");

    await client.run();
    picker.onchange = null;
  } catch (err: any) {
    painter.end(String(err?.message ?? err), false);
  } finally {
    running = false;
  }
}

$("reconnect").addEventListener("click", () => void connect());
void connect();
