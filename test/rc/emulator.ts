/**
 * The Firebase Realtime Database emulator, as the rc cases drive it.
 *
 * Gated on MAGMUX_FIREBASE_EMULATOR=1 and on a JDK 21+ being findable:
 * firebase-tools 15.19.1 refuses an older JDK and exits BEFORE binding the
 * port, so a machine whose `java` is 17 (this one, at the time of writing) sees
 * the emulator never answer rather than a message saying why. The JDK override
 * is for the CHILD only — changing a machine's default JDK is not a test's
 * business.
 *
 * The admin bypass is `Authorization: Bearer owner`. It is not in the public
 * documentation; the source is firebase-tools 15.19.1,
 * lib/emulator/hubExport.js:152-157, and the Go suite's emulator case is where
 * that assumption is actually pinned.
 */
import { spawn, type ChildProcess } from "node:child_process";
import crypto from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import { REPO, sleep } from "./harness.ts";

export const EMU_PROJECT = "demo-magmux";
export const EMU_NS = "demo-magmux-default-rtdb";
export const EMU_ADDR = "127.0.0.1:9000";
export const EMU_BASE = `http://${EMU_ADDR}`;

export function emulatorEnabled(): boolean {
  return process.env.MAGMUX_FIREBASE_EMULATOR === "1";
}

/** A JDK the firebase CLI will accept, as environment for the child only. */
export function javaEnv(): NodeJS.ProcessEnv {
  for (const home of [
    "/opt/homebrew/opt/openjdk@21",
    "/opt/homebrew/opt/openjdk@25",
    "/opt/homebrew/opt/openjdk",
    "/usr/local/opt/openjdk@21",
  ]) {
    if (fs.existsSync(path.join(home, "bin", "java"))) {
      return { JAVA_HOME: home, PATH: `${path.join(home, "bin")}:${process.env.PATH}` };
    }
  }
  return {};
}

export class Emulator {
  private proc: ChildProcess | null = null;
  private log = "";
  readonly logFile = `/tmp/magmux-rc-emulator-${process.pid}.log`;

  /** Start, and poll until it answers. The jar downloads on first run. */
  async start(timeoutMs = 300_000) {
    const dir = path.join(REPO, "examples/firebase");
    this.proc = spawn("firebase", ["emulators:start", "--only", "database", "--project", EMU_PROJECT], {
      cwd: dir,
      env: { ...process.env, ...javaEnv() },
      // Its own process group: the CLI spawns a Java child, and killing only
      // the CLI leaves the jar holding port 9000 for the next run.
      detached: true,
      stdio: ["ignore", "pipe", "pipe"],
    });
    this.proc.stdout?.on("data", (b) => (this.log += b.toString()));
    this.proc.stderr?.on("data", (b) => (this.log += b.toString()));

    const deadline = Date.now() + timeoutMs;
    while (Date.now() < deadline) {
      if (await this.ping()) return;
      if (this.proc.exitCode !== null) break;
      await sleep(250);
    }
    fs.writeFileSync(this.logFile, this.log);
    throw new Error(`the database emulator never answered on ${EMU_ADDR}\n${this.log.slice(-2000)}`);
  }

  async ping(): Promise<boolean> {
    try {
      const r = await fetch(`${EMU_BASE}/.json?ns=${EMU_NS}`, { headers: { Authorization: "Bearer owner" } });
      return r.ok;
    } catch {
      return false;
    }
  }

  stop() {
    if (!this.proc?.pid) return;
    try {
      process.kill(-this.proc.pid, "SIGTERM");
    } catch {
      try {
        this.proc.kill("SIGTERM");
      } catch {}
    }
  }

  get output(): string {
    return this.log;
  }
}

/** One REST identity against the emulator: admin, an owner uid, or a stranger. */
export class EmuREST {
  constructor(readonly auth: { bearer?: string; idToken?: string }) {}

  private url(p: string): string {
    const q = new URLSearchParams({ ns: EMU_NS });
    if (this.auth.idToken) q.set("auth", this.auth.idToken);
    return `${EMU_BASE}/${p.replace(/^\//, "")}.json?${q}`;
  }

  private headers(): Record<string, string> {
    return this.auth.bearer ? { Authorization: `Bearer ${this.auth.bearer}` } : {};
  }

  async get(p: string): Promise<any> {
    const r = await fetch(this.url(p), { headers: this.headers() });
    if (!r.ok) return null;
    const t = await r.text();
    return t === "null" || t === "" ? null : JSON.parse(t);
  }

  async put(p: string, v: unknown): Promise<number> {
    const r = await fetch(this.url(p), {
      method: "PUT",
      headers: { ...this.headers(), "Content-Type": "application/json" },
      body: JSON.stringify(v),
    });
    return r.status;
  }
}

export const admin = () => new EmuREST({ bearer: "owner" });

/**
 * An UNSIGNED Firebase ID token, in the shape @firebase/rules-unit-testing
 * produces: header {"alg":"none","typ":"JWT"}, the usual securetoken claims and
 * an EMPTY signature segment. Only an emulator accepts one; nothing here works
 * against a real project and nothing here is a credential.
 */
export function idToken(uid: string, project = EMU_PROJECT): string {
  const b64 = (o: unknown) => Buffer.from(JSON.stringify(o)).toString("base64url");
  const now = Math.floor(Date.now() / 1000);
  return (
    b64({ alg: "none", typ: "JWT" }) +
    "." +
    b64({
      iss: `https://securetoken.google.com/${project}`,
      aud: project,
      sub: uid,
      user_id: uid,
      iat: now,
      exp: now + 3600,
      auth_time: now,
      email_verified: false,
      firebase: { identities: {}, sign_in_provider: "custom" },
    }) +
    "."
  );
}

/**
 * The canonical string a command's signature is over, and the signature.
 *
 * Eight fields joined with `\n`, `args` LAST because it is the only field whose
 * content is attacker-chosen and unbounded — a newline inside it cannot shift a
 * later field into a different position. `args` is signed as the RAW JSON
 * STRING stored in the database, never as a re-encoding of the parsed value:
 * two spellings of one object have two signatures, and the one that counts is
 * the one on the wire. Mirrors transport/firebase/verify.go; the committed
 * vector in examples/firebase/hmac-vector.json is what keeps the two honest.
 */
export const SIG_PREFIX = "magmux.cmd.v1";

export function canonical(host: string, sid: string, uid: string, ts: number, nonce: string, op: string, args: string): string {
  return [SIG_PREFIX, host, sid, uid, String(ts), nonce, op, args].join("\n");
}

export function sign(key: string | Buffer, host: string, sid: string, uid: string, ts: number, nonce: string, op: string, args: string): string {
  return crypto.createHmac("sha256", key).update(canonical(host, sid, uid, ts, nonce, op, args)).digest("hex");
}

/** Check the signer above against the committed vector before trusting it. */
export function checkVector(): { ok: boolean; want: string; got: string } {
  const v = JSON.parse(fs.readFileSync(path.join(REPO, "examples/firebase/hmac-vector.json"), "utf8"));
  const got = sign(v.key, v.host, v.sid, v.uid, v.ts, v.nonce, v.op, v.args);
  return { ok: got === v.sig, want: v.sig, got };
}
