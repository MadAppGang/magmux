#!/usr/bin/env bun
/**
 * RC case 3 — validation criterion 3: the Firebase mirror, a signed command
 * executing, and an unsigned one rejected.
 *
 * Against Google's OWN Realtime Database emulator, with the rules magmux ships
 * (examples/firebase/database.rules.json) loaded, and against a real magmux
 * binary started with `--firebase <config>`. The Go suite drives the adapter
 * directly; this is the only place the CLI wiring is exercised — the flag, the
 * config load before init(), the mirror coming up after the layout, and the
 * final flush in the teardown.
 *
 * The command defence is TWO layers and both are proved here, because either
 * alone is a system that fails open:
 *
 *   1. the RULES refuse a non-owner uid. The write never lands, and magmux
 *      never sees it. That is the layer that survives magmux being wrong.
 *   2. MAGMUX refuses a bad HMAC from a real owner. The write lands, and the
 *      op does not run. That is the layer that survives the database being
 *      wrong — a stolen session, a mis-set rule, a compromised console.
 *
 * Gated on MAGMUX_FIREBASE_EMULATOR=1. It needs the firebase CLI and a JDK 21+
 * (firebase-tools 15.19.1 refuses anything older and exits before binding the
 * port), and downloads an emulator jar on first run.
 */
import fs from "node:fs";
import path from "node:path";
import {
  C, RemoteMagmux, buildMagmux, criterion, dumpStderr, header, hostname, report,
  skipped, until, writeTokenFile, type Check,
} from "./harness.ts";
import { Emulator, EmuREST, admin, checkVector, emulatorEnabled, idToken, sign } from "./emulator.ts";

header(
  "RC case 3 — Firebase: the mirror, a signed command, an unsigned one",
  "validation criterion 3, against the real RTDB emulator and the shipped rules",
);

if (!emulatorEnabled()) {
  skipped(
    3,
    "Firebase: mirror, signed command executes, unsigned refused",
    "set MAGMUX_FIREBASE_EMULATOR=1 to run it — it needs firebase-tools and a JDK 21+, " +
      "and downloads an emulator jar on first run",
  );
  process.exit(0);
}

const OWNER = "rc-owner-1";
const STRANGER = "rc-not-an-owner";
const HOST = `rc-${hostname()}`;
const ROOT = "magmux";
const KEY = "magmux-rc-emulator-key-not-a-real-secret";

const bin = buildMagmux();
const emu = new Emulator();
const m = new RemoteMagmux(bin, "fb");
const checks: Check[] = [];

try {
  // The signer here has to agree with transport/firebase/verify.go before
  // anything it produces means anything. The committed vector is the contract.
  const vec = checkVector();
  checks.push({
    label: "the harness's HMAC agrees with the committed vector",
    pass: vec.ok,
    detail: vec.ok ? `${vec.want.slice(0, 16)}…` : `got ${vec.got.slice(0, 16)}… want ${vec.want.slice(0, 16)}…`,
  });
  if (!vec.ok) throw new Error("the harness signs differently from magmux; nothing below would be meaningful");

  console.log(`\n${C.bold}  Emulator${C.reset}`);
  const t0 = Date.now();
  await emu.start();
  console.log(`  ${C.green}✓${C.reset} answering on 127.0.0.1:9000 after ${Date.now() - t0}ms`);

  // The config, written fresh: commands ON, one owner, an HMAC key in a 0600
  // file, and an allowlist that does NOT include `input`. `ticket.*` is there
  // for case 5, which reuses this shape.
  // 0600 and no group or other bits: loadKeyFile gives the HMAC key the
  // credential treatment, not the config treatment, and refuses anything laxer.
  const keyFile = path.join(m.dir, "cmd.key");
  writeTokenFile(keyFile, KEY);
  const cfgPath = path.join(m.dir, "firebase.json");
  fs.writeFileSync(
    cfgPath,
    JSON.stringify(
      {
        root: ROOT,
        host: HOST,
        emulator: { host: "127.0.0.1:9000", ns: "demo-magmux-default-rtdb" },
        frameFps: 2,
        commands: {
          enabled: true,
          owners: [OWNER],
          keyFile,
          allowOps: ["list", "capture", "send", "ticket.*"],
        },
      },
      null,
      2,
    ),
  );

  await m.start(["-e", "cat", "--label", "mirrored", "--id", `rc${process.pid}`, "--firebase", cfgPath]);
  const mirror = await m.waitForLine(/mirroring to (\S+)/, 20_000);
  const sess = new URL(mirror[1]).pathname.replace(/^\/+|\.json$/g, "");
  console.log(`\n${C.bold}  magmux${C.reset}`);
  console.log(`  ${C.grey}session path ${C.reset}${sess}`);
  console.log(`  ${C.grey}commands     ${C.reset}${/commands are ENABLED/.test(m.stderr) ? "ENABLED" : "off"}`);

  const db = admin();

  // ── 1. the mirror ────────────────────────────────────────────────────────
  console.log(`\n${C.bold}  The mirror${C.reset}`);

  let meta: any = null;
  await until("the session meta", 30_000, async () => {
    meta = await db.get(`${sess}/meta`);
    return meta?.pid === (m.proc?.pid ?? -1);
  });
  checks.push({
    label: "the session meta is mirrored",
    pass: meta?.alive === true && meta?.pid === m.proc?.pid,
    detail: `alive=${meta?.alive} pid=${meta?.pid} version=${meta?.version} ${meta?.rows}x${meta?.cols}`,
  });

  let panes: any = null;
  await until("the pane meta and state", 30_000, async () => {
    panes = await db.get(`${sess}/panes`);
    return panes?.p0?.meta?.label === "mirrored" && panes?.p0?.state != null;
  });
  checks.push({
    label: "the pane's meta and state are mirrored",
    pass: panes?.p0?.meta?.label === "mirrored" && typeof panes?.p0?.state?.state === "string",
    detail: `p0 label=${panes?.p0?.meta?.label} state=${panes?.p0?.state?.state}`,
  });

  // A frame only exists once the pane has painted, so type into it first.
  await m.http("POST", "/v1/ops/input", { body: { pane: 0, text: "MAGMUX_FB_MIRROR_OK\r" } });
  let frame: any = null;
  await until("a mirrored frame carrying what was typed", 30_000, async () => {
    frame = await db.get(`${sess}/panes/p0/frame`);
    const rows = Object.values(frame?.lines ?? {}).map((l: any) => String(l?.t ?? ""));
    return rows.some((t) => t.includes("MAGMUX_FB_MIRROR_OK"));
  });
  checks.push({
    label: "the pane's frame is mirrored, rows and all",
    pass: !!frame,
    detail: `seq=${frame?.seq} ${frame?.rows}x${frame?.cols} ${Object.keys(frame?.lines ?? {}).length} rows, key=${frame?.key}`,
  });

  const ops = await db.get(`${sess}/ops`);
  checks.push({
    label: "the op catalogue is mirrored as JSON strings",
    pass: typeof ops?.list === "string" && typeof ops?.open_pane === "string",
    detail: `${Object.keys(ops ?? {}).length} ops, each a ${typeof ops?.list}`,
  });

  const owners = await db.get(`${ROOT}/hosts/${HOST}/owners`);
  checks.push({
    label: "the owner list is mirrored ABOVE the session, where the rules read it",
    pass: owners?.[OWNER] === true,
    detail: JSON.stringify(owners),
  });

  // ── 2. a signed command from an owner executes ───────────────────────────
  console.log(`\n${C.bold}  A signed command${C.reset}`);

  const owner = new EmuREST({ idToken: idToken(OWNER) });
  const ts = Date.now();
  const nonce = "rc-nonce-000000000001";
  const args = JSON.stringify({});
  const good = {
    uid: OWNER,
    ts,
    nonce,
    op: "list",
    args,
    sig: sign(KEY, HOST, path.basename(sess), OWNER, ts, nonce, "list", args),
  };
  const started = Date.now();
  const putCode = await owner.put(`${sess}/commands/-RCgood0001`, good);
  checks.push({
    label: "the rules accept a well-formed command from an owner",
    pass: putCode === 200,
    detail: `HTTP ${putCode}`,
  });

  let result: any = null;
  await until("the command's result", 30_000, async () => {
    result = await db.get(`${sess}/results/-RCgood0001`);
    return result?.state === "done";
  });
  const execMs = Date.now() - started;
  checks.push({
    label: "magmux ran it and mirrored the result",
    pass: result?.ok === true && String(result?.result ?? "").includes("panes"),
    detail: `ok=${result?.ok} in ${execMs}ms, result=${String(result?.result ?? "").slice(0, 60)}…`,
  });
  await until("the executed command to be deleted", 20_000, async () => (await db.get(`${sess}/commands/-RCgood0001`)) == null);
  checks.push({
    label: "the command node is deleted after it runs (at most once)",
    pass: (await db.get(`${sess}/commands/-RCgood0001`)) == null,
    detail: "commands/-RCgood0001 is gone; a restart cannot re-run it",
  });

  // ── 3a. an unsigned command from an owner — magmux refuses ───────────────
  console.log(`\n${C.bold}  An unsigned command${C.reset}`);

  const tsBad = Date.now();
  const badArgs = JSON.stringify({ pane: 0, text: "MAGMUX_FB_SHOULD_NEVER_BE_TYPED\r" });
  const bad = {
    uid: OWNER,
    ts: tsBad,
    nonce: "rc-nonce-000000000002",
    op: "send",
    args: badArgs,
    // The right length and the right alphabet, and wrong. A missing `sig` would
    // be refused by the rules' .validate, which is a different layer.
    sig: "0".repeat(64),
  };
  const badPut = await owner.put(`${sess}/commands/-RCbad00002`, bad);
  checks.push({
    label: "the rules accept it (they do not check the HMAC — magmux does)",
    pass: badPut === 200,
    detail: `HTTP ${badPut}`,
  });

  let badResult: any = null;
  await until("magmux to reject the bad signature", 30_000, async () => {
    badResult = await db.get(`${sess}/results/-RCbad00002`);
    return badResult?.state === "done";
  });
  checks.push({
    label: "magmux refuses it with `unauthorized`",
    pass: badResult?.ok === false && badResult?.code === "unauthorized",
    detail: `ok=${badResult?.ok} code=${badResult?.code} ${String(badResult?.error ?? "").slice(0, 50)}`,
  });

  const screen = await m.http("GET", "/v1/panes/0/screen");
  checks.push({
    label: "nothing from the unsigned command reached the PTY",
    pass: !String(screen.json?.text ?? "").includes("MAGMUX_FB_SHOULD_NEVER_BE_TYPED"),
    detail: `${String(screen.json?.text ?? "").length} bytes of screen, marker absent`,
  });

  // ── 3b. a non-owner uid — the RULES refuse it ────────────────────────────
  const stranger = new EmuREST({ idToken: idToken(STRANGER) });
  const tsS = Date.now();
  const sArgs = JSON.stringify({});
  const forged = {
    uid: STRANGER,
    ts: tsS,
    nonce: "rc-nonce-000000000003",
    op: "list",
    args: sArgs,
    // Correctly signed, even. The rules never get as far as caring.
    sig: sign(KEY, HOST, path.basename(sess), STRANGER, tsS, "rc-nonce-000000000003", "list", sArgs),
  };
  const forgedPut = await stranger.put(`${sess}/commands/-RCforge0003`, forged);
  checks.push({
    label: "a non-owner uid is refused by the RULES, before magmux sees it",
    pass: forgedPut === 401 && (await db.get(`${sess}/commands/-RCforge0003`)) == null,
    detail: `HTTP ${forgedPut}, and nothing reached the database`,
  });

  // An owner may not spoof another uid either: the rule pins uid === auth.uid.
  const spoofPut = await owner.put(`${sess}/commands/-RCspoof0004`, { ...forged, uid: STRANGER });
  checks.push({
    label: "an owner cannot write a command claiming another uid",
    pass: spoofPut === 400 || spoofPut === 401,
    detail: `HTTP ${spoofPut} (.validate pins uid === auth.uid)`,
  });

  // ── 4. teardown writes alive:false ───────────────────────────────────────
  console.log(`\n${C.bold}  Teardown${C.reset}`);
  await m.stop();
  let finalMeta: any = null;
  await until("the final flush", 20_000, async () => {
    finalMeta = await db.get(`${sess}/meta`);
    return finalMeta?.alive === false;
  });
  checks.push({
    label: "the final flush writes alive:false",
    pass: finalMeta?.alive === false,
    detail: `alive=${finalMeta?.alive} endedAt=${finalMeta?.endedAt ?? "—"}`,
  });
} catch (err) {
  checks.push({ label: "the case ran to completion", pass: false, detail: String(err) });
  dumpStderr(m);
} finally {
  await m.stop();
  m.cleanup();
  emu.stop();
}

const code = report(checks);
criterion(
  3,
  "Firebase: mirror, signed command executes, unsigned refused",
  code === 0,
  `${checks.filter((c) => c.pass).length}/${checks.length} checks`,
);
process.exit(code);
