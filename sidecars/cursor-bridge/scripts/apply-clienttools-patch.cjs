#!/usr/bin/env node
// Idempotent, anchor-guarded, version-pinned patcher for @cursor/sdk.
//
// Purpose: route EVERY tool the Cursor agent runs to Claude Code (the end user's
// machine) instead of executing it on the sidecar, and never touch the sidecar's
// filesystem. We do this at the SDK's single server-side tool-dispatch seam:
// the unary (class N) and stream (class P) adapters both funnel through
// `exec.execute(...)`, keyed by the stable protobuf oneof case `t.message.case`.
//
// Three tiny replacements (all logic lives in the sidecar via globalThis, so the
// patch stays minimal and robust):
//   1. serializeResult/serializeStream factory `$`: when the result carries
//      `__ccJson`, build the proper protobuf result via `<MsgType>.fromJson(...)`
//      (handles nested oneofs correctly — plain partial objects silently drop them).
//      It also publishes the live `$` as `globalThis.__CC_SELFTEST_SERIALIZE` so a
//      startup self-test (ADD-74) can drive real result payloads through this exact
//      seam and fail-fast if fromJson cannot construct the SDK shape.
//   2. unary exec call -> `globalThis.__CC_EXEC_U` (falls back to native if unset).
//   3. stream exec call -> `globalThis.__CC_EXEC_S` (falls back to native if unset).
//
// If the globals are not installed (e.g. SDK used standalone), behavior is native.
//
// On any anchor mismatch or version drift the patcher fails LOUD (non-zero exit)
// so a new SDK release is re-verified before shipping. The SHA-256 of the pristine
// bundle is gated FAIL-CLOSED (M27): a drifted byte-stream aborts the patch unless an
// explicit development escape hatch is set, BEFORE any anchor replacement runs.

const fs = require("fs");
const path = require("path");
const crypto = require("crypto");

const PINNED_VERSION = "1.0.14";
// SHA-256 of the pristine dist/cjs/index.js the anchors were verified against. On any SDK bump this
// changes — re-run anchor verification, then update PINNED_VERSION and this hash. The patcher logs the
// observed hash every run so a reviewer can confirm they patched the exact byte-stream.
const EXPECTED_BUNDLE_SHA256 = "fe49583a88f280b6efa32729dd724d10d5a07a04fcc1cdb16a75733678a8d7db";
const sdkRoot = path.join(__dirname, "..", "node_modules", "@cursor", "sdk");
const target = path.join(sdkRoot, "dist", "cjs", "index.js");
// An already-patched bundle is detected as stale WITHOUT bumping this marker: the anchor `from` strings
// (pristine forms) won't match an already-patched bundle, so the anchor-count check fails loud (and the SHA
// gate also rejects a non-pristine byte-stream), forcing a pristine reinstall (npm ci) before re-patching.
// CONTRACT: this MARK is a cross-file shared constant — the bridge's assertPatched() greps for this exact
// substring. Changing it (e.g. to a "-v2") REQUIRES updating assertPatched() in cursor-agent-bridge.mjs in
// lockstep, or loadSdk() will reject the freshly-patched bundle. So do NOT bump it for a patch-shape change
// alone (the anchor/SHA gates already catch a stale bundle); only bump it alongside the bridge-side grep.
//
// STALENESS GUARD (ADD-102 / Comment 7): the MARK alone does NOT prove a bundle is current. A partial/stale
// patch (marker written but a seam missing — e.g. a half-applied patch, a hand-edited vendor bundle, or a
// cached artifact copied with the marker but minus the advertise/serialize seam) would otherwise be accepted
// by a marker-only short-circuit and start the bridge with a broken seam (no tools advertised, or malformed
// result deserialization) as a silent behavior bug rather than a startup error. We keep MARK at -v1 and use
// the CAPABILITY check below (REQUIRED_SEAM_TOKENS) as the structural staleness guard instead of bumping the
// marker: adding a new patch seam therefore does NOT require a marker bump (which would force a lockstep
// bridge assertPatched() change), but it DOES require adding that seam's sentinel token to
// REQUIRED_SEAM_TOKENS so the capability check keeps rejecting bundles that pre-date the seam.
const MARK = "/*cursor-composer-clienttools-patched-v1*/";

// CAPABILITY tokens (ADD-102 / Comment 7): the sentinel substrings every CURRENT patched bundle MUST contain.
// A bundle that starts with MARK is only treated as already-patched (idempotent re-run) when ALL of these are
// present; if MARK is present but any token is missing the bundle is partially/stalely patched and we fail
// CLOSED with a 'stale-bundle' PatchError (never exit 0). These mirror the globals the bridge installs and
// relies on (assertPatched()/loadSdk()/the self-tests): the serializer-capture seam (__CC_SELFTEST_SERIALIZE,
// edit 1), the unary+stream dispatch self-test harness (__CC_SELFTEST_DISPATCH_U/S, appended), and the
// advertise/mcp_tools seam (__CC_GET_ADVERTISE__, edit 4). Keep this in sync with the `edits`/harness above:
// any new seam => add its sentinel token here (no marker bump needed — see the MARK comment).
const REQUIRED_SEAM_TOKENS = [
  "__CC_SELFTEST_SERIALIZE",
  "__CC_SELFTEST_DISPATCH_U",
  "__CC_SELFTEST_DISPATCH_S",
  "__CC_GET_ADVERTISE__",
];

// Development escape hatches that downgrade the M27 SHA fail-closed gate to a warning. EITHER name is
// honored (the audit/spec canonical name plus an alternate); these MUST stay opt-in so production never
// patches a drifted byte-stream by default. They do NOT relax the version pin or the anchor-count checks.
const SHA_OVERRIDE_ENV = ["CURSOR_CLIENTTOOLS_ALLOW_UNVERIFIED_SDK", "CURSOR_SDK_PATCH_ALLOW_SHA_MISMATCH"];

// A typed failure the CLI wrapper maps to a non-zero exit and callers/tests can assert on by `code`.
// Carrying a code (instead of a bare string) lets the self-tests prove the SHA gate fails CLOSED rather
// than relying on stdout text matching.
class PatchError extends Error {
  constructor(code, message) {
    super(message);
    this.name = "PatchError";
    this.code = code;
  }
}

function shaOverrideEnabled(env) {
  return SHA_OVERRIDE_ENV.some((k) => env[k] === "1");
}

const edits = [
  {
    // ADD-74: capture the LIVE patched `$` factory onto a global so the bridge can drive representative
    // result `__ccJson` payloads through the REAL serializer at startup (selfTestResultSerialize), proving
    // the fromJson seam constructs the SDK protobuf shape — not merely that dispatch is intercepted. `$` and
    // its closed-over `I.yT` (ExecClientMessage) are module-internal, so the capture MUST sit next to the
    // definition (the appended tail harness cannot see them). Appended to `to` only; `from` is unchanged so a
    // pristine bundle still matches, and the `expect:1` count below still guards the single seam.
    name: "serializeResult/serializeStream fromJson",
    expect: 1,
    from: `function $(e){return function(t,n){const r={case:e,value:n};return new I.yT({id:t,message:r})}}`,
    to: `function $(e){return function(t,n){if(n&&typeof n==="object"&&"__ccJson"in n){var __j=n.__ccJson,__f=I.yT.fields.list().find(function(f){return f.localName===e||f.name===e});if(__f&&__f.T){try{n=__f.T.fromJson(__j)}catch(__err){throw new Error("[clienttools] sidecar sent invalid result shape for "+e+": "+((__err&&__err.message)||__err))}}else{throw new Error("[clienttools] unknown result case "+e+" (no ExecClientMessage field) -- sidecar mapping out of sync with SDK")}}var r={case:e,value:n};return new I.yT({id:t,message:r})}}try{globalThis.__CC_SELFTEST_SERIALIZE=$}catch(__e){}`,
  },
  {
    name: "unary tool dispatch -> CC",
    expect: 1,
    from: `o=await n.exec.execute(e,s,{execId:t.execId})`,
    to: `o=await(globalThis.__CC_EXEC_U?globalThis.__CC_EXEC_U(n,e,s,t):(globalThis.__CC_ALLOW_NATIVE?n.exec.execute(e,s,{execId:t.execId}):Promise.reject(new Error("[clienttools] no bridge installed (set globalThis.__CC_ALLOW_NATIVE to allow native sidecar exec) -- refusing"))))`,
  },
  {
    name: "stream tool dispatch -> CC",
    expect: 1,
    from: `o=n.exec.execute(e,s,{execId:t.execId});for await`,
    to: `o=(globalThis.__CC_EXEC_S?globalThis.__CC_EXEC_S(n,e,s,t):(globalThis.__CC_ALLOW_NATIVE?n.exec.execute(e,s,{execId:t.execId}):(async function*(){throw new Error("[clienttools] no bridge installed (set globalThis.__CC_ALLOW_NATIVE to allow native sidecar exec) -- refusing")})()));for await`,
  },
  {
    // Advertise the CLIENT's tools (incl. MCP tools) to the Cursor model by appending them to the
    // run request's mcp_tools. __CC_GET_ADVERTISE__ reads the current session's tool inventory
    // (via AsyncLocalStorage) so concurrent sessions stay isolated. No MCP server/child-process.
    name: "advertise client tools as mcp_tools",
    expect: 1,
    from: `=c.map((e=>({name:e.name,providerIdentifier`,
    to: `=(Array.isArray(c)?c.slice():[]).concat(globalThis.__CC_GET_ADVERTISE__?globalThis.__CC_GET_ADVERTISE__():[]).map((e=>({name:e.name,providerIdentifier`,
  },
];

// Injected strings are written with latin1 (to byte-preserve the surrounding bundle). A codepoint > 0xFF
// (e.g. an em-dash U+2014) would be silently truncated to its low byte and corrupt the seam, so require all
// injected content to be ASCII and fail loud otherwise.
function assertAscii(label, str) {
  for (let i = 0; i < str.length; i++) {
    if (str.charCodeAt(i) > 0x7f) {
      throw new PatchError(
        "non-ascii-injection",
        `injected string "${label}" has a non-ASCII char U+${str.charCodeAt(i).toString(16)} at index ${i}; the latin1 write would corrupt it -- use ASCII.`,
      );
    }
  }
}

// Core, side-effect-free patch logic. Takes the pristine bundle text + SDK version + an env map and returns
// the patched text plus the messages the CLI should print, OR throws a PatchError on any fail-closed
// condition. Kept pure (no fs/process) so the self-tests can drive the SHA gate and anchor checks against
// synthetic bundles without installing the real @cursor/sdk.
//
// Order is load-bearing (M27): version pin -> marker + CAPABILITY check (already-patched short-circuit ONLY
// when MARK present AND every required seam present; MARK present + a seam missing -> fail-closed 'stale-bundle')
// -> SHA gate (fail-closed unless overridden) -> ASCII assertions -> anchor-count checks -> apply -> append
// self-test harness. The SHA gate MUST precede the anchor checks: a drifted bundle whose anchors happen to
// still match must NOT be patched silently, because the surrounding semantics may have changed (worst case:
// an unpatched seam reintroduces native sidecar execution). The marker+capability check stays BEFORE the SHA
// gate because an already-patched bundle's bytes will not match the pristine SHA (it is post-patch) — but a
// MARKED bundle that is missing a seam must still fail loud rather than be hashed against the pristine value.
function applyPatch({ src, version, env = {} }) {
  const messages = [];

  if (typeof src !== "string") {
    throw new PatchError("bad-input", "applyPatch requires the bundle source as a latin1 string");
  }

  if (version !== PINNED_VERSION) {
    throw new PatchError(
      "version-mismatch",
      `@cursor/sdk is ${version} but patch is pinned to ${PINNED_VERSION}. ` +
        `Re-verify the anchors against the new bundle, then bump PINNED_VERSION.`,
    );
  }

  // ADD-102 / Comment 7: marker + CAPABILITY check (replaces the old marker-only short-circuit). A bundle that
  // starts with MARK is only idempotently already-patched when it ALSO carries every required seam token; if
  // any is missing the bundle is partially/stalely patched (marker written but a seam dropped) and we fail
  // CLOSED with 'stale-bundle' — never exit 0 on a half-patched bundle (which would start the bridge with a
  // broken seam as a silent behavior bug). This is the static counterpart to the bridge's runtime self-tests.
  if (src.startsWith(MARK)) {
    for (const token of REQUIRED_SEAM_TOKENS) {
      if (!src.includes(token)) {
        throw new PatchError(
          "stale-bundle",
          `marker present but seam ${token} missing — the @cursor/sdk bundle is partially/stalely patched; ` +
            "run `npm ci` or reinstall the SDK, then re-run the patcher",
        );
      }
    }
    return { alreadyPatched: true, patchedSrc: src, observedSha: null, messages: [{ level: "log", text: "already patched" }] };
  }

  // --- M27: fail-closed SHA gate, BEFORE the anchor-count checks below. ---
  const observedSha = crypto.createHash("sha256").update(src, "latin1").digest("hex");
  if (observedSha !== EXPECTED_BUNDLE_SHA256) {
    if (!shaOverrideEnabled(env)) {
      // Hard-fail: the byte-stream the anchors were verified against has changed. Do NOT proceed to the
      // anchor replacements — a coincidental anchor match on a semantically-different bundle could ship a
      // partially-patched (or native-exec-leaking) SDK.
      throw new PatchError(
        "sha-mismatch",
        `pristine bundle sha256=${observedSha} != recorded ${EXPECTED_BUNDLE_SHA256}. @cursor/sdk has changed ` +
          `(or was already modified) — re-verify the anchors, then update PINNED_VERSION + EXPECTED_BUNDLE_SHA256. ` +
          `Refusing to patch a drifted bundle. To override for local development only, set ` +
          `${SHA_OVERRIDE_ENV[0]}=1 (or ${SHA_OVERRIDE_ENV[1]}=1).`,
      );
    }
    // Override set: development-only escape hatch. The anchor-count checks below still guard structurally,
    // but the operator has explicitly accepted an unverified byte-stream.
    messages.push({
      level: "warn",
      text:
        `WARNING: pristine bundle sha256=${observedSha} != recorded ${EXPECTED_BUNDLE_SHA256}, but ` +
        `${SHA_OVERRIDE_ENV.find((k) => env[k] === "1")}=1 is set — proceeding with an UNVERIFIED @cursor/sdk ` +
        `(development override). Re-verify anchors and update the recorded hash before shipping.`,
    });
  } else {
    messages.push({ level: "log", text: `pristine bundle sha256 verified (${observedSha.slice(0, 16)}…)` });
  }

  for (const ed of edits) assertAscii(ed.name + ".to", ed.to);

  // --- anchor-count checks (structural guard); only reached AFTER the SHA gate. ---
  for (const ed of edits) {
    const count = src.split(ed.from).length - 1;
    if (count !== ed.expect) {
      throw new PatchError(
        "anchor-mismatch",
        `anchor "${ed.name}": found ${count}, expected ${ed.expect}. SDK bundle changed — re-verify.`,
      );
    }
  }

  let out = src;
  for (const ed of edits) out = out.split(ed.from).join(ed.to);

  // Bundle-level self-test harness: expose the EXACT unary/stream seam expressions (the same strings just
  // applied at the real dispatch sites) as globals, so the bridge can drive the real patched dispatch logic
  // at startup and prove native exec.execute() is unreachable. Derived from the same `to` strings, so the
  // harness can never drift from the live seam; the anchor-count checks above guarantee the live seam exists.
  const unaryExpr = edits.find((e) => e.name === "unary tool dispatch -> CC").to.replace(/^o=await/, "");
  const streamExpr = edits.find((e) => e.name === "stream tool dispatch -> CC").to.replace(/^o=/, "").replace(/;for await$/, "");
  const harness =
    `\n;globalThis.__CC_SELFTEST_DISPATCH_U=function(n,e,s,t){return Promise.resolve(${unaryExpr})};` +
    `globalThis.__CC_SELFTEST_DISPATCH_S=function(n,e,s,t){return ${streamExpr}};\n`;
  assertAscii("self-test harness", harness);
  out += harness;

  out = MARK + out;

  messages.push({
    level: "log",
    text: `patched @cursor/sdk@${version}: CC tool routing (unary+stream) + fromJson result construction`,
  });

  return { alreadyPatched: false, patchedSrc: out, observedSha, messages };
}

function emit(messages) {
  for (const m of messages) {
    if (m.level === "warn") console.warn(`[clienttools] ${m.text}`);
    else if (m.level === "error") console.error(`[clienttools] ${m.text}`);
    else console.log(`[clienttools] ${m.text}`);
  }
}

// CLI entrypoint: read the real bundle, run the pure core, write the result. Preserves the original
// stdout/stderr/exit contract exactly (postinstall depends on it). A PatchError is mapped to a single
// `[clienttools] <msg>` on stderr + non-zero exit (fail-loud / fail-closed).
function main() {
  function fail(msg) {
    console.error(`[clienttools] ${msg}`);
    process.exit(1);
  }

  let version;
  try {
    version = require(path.join(sdkRoot, "package.json")).version;
  } catch (e) {
    return fail(`cannot read @cursor/sdk package.json: ${e.message}`);
  }

  let src;
  try {
    src = fs.readFileSync(target, "latin1");
  } catch (e) {
    return fail(`cannot read bundle ${target}: ${e.message}`);
  }

  let result;
  try {
    result = applyPatch({ src, version, env: process.env });
  } catch (e) {
    if (e instanceof PatchError) return fail(e.message);
    return fail(`unexpected patch failure: ${e && e.message ? e.message : e}`);
  }

  emit(result.messages);
  if (result.alreadyPatched) {
    process.exit(0);
  }

  try {
    fs.writeFileSync(target, result.patchedSrc, "latin1");
  } catch (e) {
    return fail(`cannot write bundle: ${e.message}`);
  }
}

if (require.main === module) {
  main();
}

module.exports = {
  applyPatch,
  PatchError,
  PINNED_VERSION,
  EXPECTED_BUNDLE_SHA256,
  MARK,
  REQUIRED_SEAM_TOKENS,
  SHA_OVERRIDE_ENV,
  edits,
};
