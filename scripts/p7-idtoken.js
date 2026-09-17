// Mint an UNSIGNED Firebase ID token for the RTDB emulator, in the shape
// @firebase/rules-unit-testing produces: header {"alg":"none","typ":"JWT"}, the
// usual securetoken claims, and an EMPTY signature segment.
//
// It is only ever accepted by an emulator. Nothing here works against a real
// project, and nothing here is a credential.
//
//   node scripts/p7-idtoken.js <uid> <projectId>
const [, , uid, project] = process.argv;
if (!uid || !project) {
  console.error("usage: node p7-idtoken.js <uid> <projectId>");
  process.exit(2);
}
const b64 = (o) => Buffer.from(JSON.stringify(o)).toString("base64url");
const now = Math.floor(Date.now() / 1000);
const header = { alg: "none", typ: "JWT" };
const payload = {
  iss: `https://securetoken.google.com/${project}`,
  aud: project,
  sub: uid,
  user_id: uid,
  iat: now,
  exp: now + 3600,
  auth_time: now,
  email_verified: false,
  firebase: { identities: {}, sign_in_provider: "custom" },
};
process.stdout.write(`${b64(header)}.${b64(payload)}.`);
