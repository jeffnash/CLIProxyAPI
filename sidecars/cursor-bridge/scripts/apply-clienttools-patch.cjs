#!/usr/bin/env node
// Idempotent, anchor-guarded, version-pinned patcher for @cursor/sdk.
//
// Purpose: route EVERY tool the Cursor agent runs to Claude Code (the end user's
// machine) instead of executing it on the sidecar, and never touch the sidecar's
// filesystem. We do this at the SDK's single server-side tool-dispatch seam:
// the unary (class _) and stream (class P) adapters both funnel through
// `exec.execute(...)`, keyed by the stable protobuf oneof case `t.message.case`.
//
// As of @cursor/sdk@1.0.23 the local-executor (and these seams) live in the
// webpack async chunk dist/cjs/973.js, NOT the main dist/cjs/index.js entry.
// The main entry only lazy-loads that chunk via a.e(973) on createLocalExecutor.
// This patcher therefore:
//   1. Patches 973.js (the four anchors + MARK + dispatch self-test harness).
//   2. Stamps index.js with MARK + an eager-load of the local-executor chunk so
//      requiring @cursor/sdk installs the seam globals (needed by loadSdk /
//      startup self-tests that run before any agent turn).
//
// Four tiny replacements (all logic lives in the sidecar via globalThis, so the
// patch stays minimal and robust):
//   1. serializeResult/serializeStream factory `U`: when the result carries
//      `__ccJson`, build the proper protobuf result via `<MsgType>.fromJson(...)`
//      (handles nested oneofs correctly — plain partial objects silently drop them).
//      It also publishes the live `U` as `globalThis.__CC_SELFTEST_SERIALIZE` so a
//      startup self-test (ADD-74) can drive real result payloads through this exact
//      seam and fail-fast if fromJson cannot construct the SDK shape.
//   2. unary exec call -> `globalThis.__CC_EXEC_U` (falls back to native if unset).
//   3. stream exec call -> `globalThis.__CC_EXEC_S` (falls back to native if unset).
//   4. advertise client tools as mcp_tools via globalThis.__CC_GET_ADVERTISE__.
//
// If the globals are not installed (e.g. SDK used standalone), behavior is native
// only when __CC_ALLOW_NATIVE is set; otherwise dispatch refuses.
//
// On any anchor mismatch or version drift the patcher fails LOUD (non-zero exit)
// so a new SDK release is re-verified before shipping. The SHA-256 of the pristine
// 973.js chunk is gated FAIL-CLOSED (M27): a drifted byte-stream aborts the patch
// unless an explicit development escape hatch is set, BEFORE any anchor replacement runs.
//
// ── Notes for the NEXT @cursor/sdk bump ──────────────────────────────────────
// 1. Chunk id may change. 1.0.23 put the seams in webpack chunk 973.js (see BUNDLE_REL).
//    On a new release, re-scan dist/cjs/*.js for `.exec.execute` + `hookContextCollector`
//    (and/or `providerIdentifier` advertise map) — do NOT assume 973.js forever.
//    Update BUNDLE_REL + EXPECTED_BUNDLE_SHA256 + PINNED_VERSION together.
// 2. Unary site now passes `hookContextCollector` in the execute options. Keep that in
//    the native-fallback path of the unary `to` string (and declare `var i=[]` in the
//    DISPATCH self-test harness so the positive-control native path does not
//    ReferenceError under "use strict").
// 3. The `to` strings MUST track minified free-var names in the live site (r/e/o/t,
//    factory `U`, `h.yT`, advertise array `u`, …). Bridge injection semantics stay the
//    same, but a pure `from`-only update is not enough when the minifier renames locals
//    — adapt `to` (and the harness param list) in lockstep, then re-run the self-tests.
// 4. Index eager-load stamp (INDEX_EAGER_LOAD) also embeds chunk ids 745+973. If those
//    change, re-read createLocalExecutor's `a.e(...)` pair in index.js and update the
//    stamp. Inject MUST stay a comma-expression IIFE (the slot is `...,module.exports=o`).
// 5. After updating anchors: run patcher twice (exit 0 both), `npm test`, `npm run selftest`,
//    `node --check` on the patched chunk + index, and confirm seam markers +
//    __CC_SELFTEST_* globals install on require("@cursor/sdk").

const fs = require("fs");
const path = require("path");
const crypto = require("crypto");

const PINNED_VERSION = "1.0.23";
// SHA-256 of the pristine dist/cjs/973.js (local-executor chunk) the anchors were verified against.
// On any SDK bump this changes — re-verify anchors (chunk id may also change; see "Notes for the
// NEXT @cursor/sdk bump" above), then update PINNED_VERSION, BUNDLE_REL, and this hash.
// The patcher logs the observed hash every run.
const EXPECTED_BUNDLE_SHA256 = "829ced604bb88908e49fcf5cd31eb22bce4e57d32074b2846d86a6c5afa26881";
// Relative path of the chunk that holds the tool-dispatch seams (minified local-executor).
// 1.0.14 lived in dist/cjs/index.js; 1.0.23 split it into webpack chunk 973.
// Chunk id is NOT stable across SDK releases — re-discover on bump (see notes above).
const BUNDLE_REL = path.join("dist", "cjs", "973.js");
const INDEX_REL = path.join("dist", "cjs", "index.js");
const sdkRoot = path.join(__dirname, "..", "node_modules", "@cursor", "sdk");
const target = path.join(sdkRoot, BUNDLE_REL);
const indexTarget = path.join(sdkRoot, INDEX_REL);
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

// Idempotent stamp on the main entry so require("@cursor/sdk") eagerly evaluates the patched local-executor
// chunk (installs __CC_SELFTEST_* + live dispatch sites). Without this, seams only load on first agent turn.
// MUST be an expression (comma-expression slot before module.exports=o), not a bare try/catch statement.
const INDEX_EAGER_MARK = "/*cursor-composer-clienttools-eager-v1*/";
// a is the webpack require; a.e(N) sync-requires ./N.js under the CJS runtime; a(modId) evaluates the module.
// Chunk ids 745+973 are the createLocalExecutor dependency pair for @cursor/sdk@1.0.23.
const INDEX_EAGER_LOAD =
  INDEX_EAGER_MARK +
  '(()=>{try{a.e(745);a.e(973);a("./src/agent/local-executor.ts")}catch(__e){}})(),';

// CAPABILITY tokens (ADD-102 / Comment 7): the sentinel substrings every CURRENT patched bundle MUST contain.
// A bundle that starts with MARK is only treated as already-patched (idempotent re-run) when ALL of these are
// present; if MARK is present but any token is missing the bundle is partially/stalely patched and we fail
// CLOSED with a 'stale-bundle' PatchError (never exit 0). These mirror the globals the bridge installs and
// relies on (assertPatched()/loadSdk()/the self-tests): the serializer-capture seam (__CC_SELFTEST_SERIALIZE,
// edit 1), the unary+stream dispatch self-test harness (__CC_SELFTEST_DISPATCH_U/S, appended), and the
// advertise/mcp_tools seam (__CC_GET_ADVERTISE__, edit 4). Keep this in sync with the `edits`/harness above:
// any new seam => add its sentinel token here (no marker bump needed — see the MARK comment).
//
// NOTE: token PRESENCE alone cannot witness the two REAL dispatch sites (edits[1]/edits[2]) — the injected
// `__CC_EXEC_U`/`__CC_EXEC_S` identifiers also appear in the appended harness UNCONDITIONALLY. The live
// dispatch sites are instead guarded in REVERSE (their pristine native `from` must be ABSENT) via
// DISPATCH_EDIT_NAMES in the already-patched short-circuit; see that branch in applyPatch.
const REQUIRED_SEAM_TOKENS = [
  "__CC_SELFTEST_SERIALIZE",
  "__CC_SELFTEST_DISPATCH_U",
  "__CC_SELFTEST_DISPATCH_S",
  "__CC_GET_ADVERTISE__",
];

// ADD-102 (live-site staleness): the names of the two REAL dispatch edits (edits[1]/edits[2]). These are the
// most security-critical seams — their absence reintroduces native sidecar exec — yet REQUIRED_SEAM_TOKENS
// above can only witness the serialize/advertise seams and the APPENDED dispatch self-test harness, NOT
// whether the live call sites were actually replaced (their injected `__CC_EXEC_U`/`__CC_EXEC_S` identifiers
// also appear verbatim in the appended harness). The already-patched short-circuit therefore additionally
// asserts these edits' pristine `from` (native-call) strings are ABSENT (see applyPatch). Anchored by name
// to the `edits` table (resolved at call time, after `edits` is declared) so the witness can never drift.
const DISPATCH_EDIT_NAMES = ["unary tool dispatch -> CC", "stream tool dispatch -> CC"];

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
    // ADD-74: capture the LIVE patched `U` factory onto a global so the bridge can drive representative
    // result `__ccJson` payloads through the REAL serializer at startup (selfTestResultSerialize), proving
    // the fromJson seam constructs the SDK protobuf shape — not merely that dispatch is intercepted. `U` and
    // its closed-over `h.yT` (ExecClientMessage) are module-internal, so the capture MUST sit next to the
    // definition (the appended tail harness cannot see them). Appended to `to` only; `from` is unchanged so a
    // pristine bundle still matches, and the `expect:1` count below still guards the single seam.
    //
    // v1.0.23 minifier renames: `$` -> `U`, `I.yT` -> `h.yT`, params `(t,n)`/`r` -> `(t,r)`/`n`.
    name: "serializeResult/serializeStream fromJson",
    expect: 1,
    from: `function U(e){return function(t,r){const n={case:e,value:r};return new h.yT({id:t,message:n})}}`,
    to: `function U(e){return function(t,r){if(r&&typeof r==="object"&&"__ccJson"in r){var __j=r.__ccJson,__f=h.yT.fields.list().find(function(f){return f.localName===e||f.name===e});if(__f&&__f.T){try{r=__f.T.fromJson(__j)}catch(__err){throw new Error("[clienttools] sidecar sent invalid result shape for "+e+": "+((__err&&__err.message)||__err))}}else{throw new Error("[clienttools] unknown result case "+e+" (no ExecClientMessage field) -- sidecar mapping out of sync with SDK")}}var n={case:e,value:r};return new h.yT({id:t,message:n})}}try{globalThis.__CC_SELFTEST_SERIALIZE=U}catch(__e){}`,
  },
  {
    // v1.0.23: unary site gained hookContextCollector; free vars are r/e/o/t (was n/e/s/t) and result is `a`.
    name: "unary tool dispatch -> CC",
    expect: 1,
    from: `a=await r.exec.execute(e,o,{execId:t.execId,hookContextCollector:i})`,
    to: `a=await(globalThis.__CC_EXEC_U?globalThis.__CC_EXEC_U(r,e,o,t):(globalThis.__CC_ALLOW_NATIVE?r.exec.execute(e,o,{execId:t.execId,hookContextCollector:i}):Promise.reject(new Error("[clienttools] no bridge installed (set globalThis.__CC_ALLOW_NATIVE to allow native sidecar exec) -- refusing"))))`,
  },
  {
    // v1.0.23: stream free vars r/e/o/t; result binding is `i` (was `o` on n).
    name: "stream tool dispatch -> CC",
    expect: 1,
    from: `i=r.exec.execute(e,o,{execId:t.execId});for await`,
    to: `i=(globalThis.__CC_EXEC_S?globalThis.__CC_EXEC_S(r,e,o,t):(globalThis.__CC_ALLOW_NATIVE?r.exec.execute(e,o,{execId:t.execId}):(async function*(){throw new Error("[clienttools] no bridge installed (set globalThis.__CC_ALLOW_NATIVE to allow native sidecar exec) -- refusing")})()));for await`,
  },
  {
    // Advertise the CLIENT's tools (incl. MCP tools) to the Cursor model by appending them to the
    // run request's mcp_tools. __CC_GET_ADVERTISE__ reads the current session's tool inventory
    // (via AsyncLocalStorage) so concurrent sessions stay isolated. No MCP server/child-process.
    // v1.0.23: source array minified name is `u` (was `c`).
    name: "advertise client tools as mcp_tools",
    expect: 1,
    from: `=u.map((e=>({name:e.name,providerIdentifier`,
    to: `=(Array.isArray(u)?u.slice():[]).concat(globalThis.__CC_GET_ADVERTISE__?globalThis.__CC_GET_ADVERTISE__():[]).map((e=>({name:e.name,providerIdentifier`,
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

function findEdit(name) {
  const ed = edits.find((e) => e.name === name);
  if (!ed) throw new PatchError("internal", `edit not found: ${name}`);
  return ed;
}

function assertRequiredSeamsPresent(src) {
  for (const token of REQUIRED_SEAM_TOKENS) {
    if (!src.includes(token)) {
      throw new PatchError(
        "stale-bundle",
        `marker present but seam ${token} missing — the @cursor/sdk bundle is partially/stalely patched; ` +
          "run `npm ci` or reinstall the SDK, then re-run the patcher",
      );
    }
  }
}

function assertDispatchSitesPatched(src) {
  for (const name of DISPATCH_EDIT_NAMES) {
    const ed = findEdit(name);
    if (src.includes(ed.from)) {
      throw new PatchError(
        "stale-bundle",
        `marker present but the "${name}" dispatch site is still in its pristine native form — the ` +
          "@cursor/sdk bundle is partially/stalely patched (native sidecar exec would be reintroduced); " +
          "run `npm ci` or reinstall the SDK, then re-run the patcher",
      );
    }
  }
}

// ADD-102 / Comment 7: marker + CAPABILITY check (replaces the old marker-only short-circuit).
function tryAlreadyPatchedShortCircuit(src) {
  if (!src.startsWith(MARK)) return null;
  assertRequiredSeamsPresent(src);
  assertDispatchSitesPatched(src);
  return { alreadyPatched: true, patchedSrc: src, observedSha: null, messages: [{ level: "log", text: "already patched" }] };
}

// M27: fail-closed SHA gate (development override downgrades to a warning).
function verifyPristineBundleSha(src, env) {
  const messages = [];
  const observedSha = crypto.createHash("sha256").update(src, "latin1").digest("hex");
  if (observedSha !== EXPECTED_BUNDLE_SHA256) {
    if (!shaOverrideEnabled(env)) {
      throw new PatchError(
        "sha-mismatch",
        `pristine bundle sha256=${observedSha} != recorded ${EXPECTED_BUNDLE_SHA256}. @cursor/sdk has changed ` +
          `(or was already modified) — re-verify the anchors, then update PINNED_VERSION + EXPECTED_BUNDLE_SHA256. ` +
          `Refusing to patch a drifted bundle. To override for local development only, set ` +
          `${SHA_OVERRIDE_ENV[0]}=1 (or ${SHA_OVERRIDE_ENV[1]}=1).`,
      );
    }
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
  return { observedSha, messages };
}

function assertAnchorCounts(src) {
  for (const ed of edits) {
    const count = src.split(ed.from).length - 1;
    if (count !== ed.expect) {
      throw new PatchError(
        "anchor-mismatch",
        `anchor "${ed.name}": found ${count}, expected ${ed.expect}. SDK bundle changed — re-verify.`,
      );
    }
  }
}

function replaceAnchors(src) {
  let out = src;
  for (const ed of edits) out = out.split(ed.from).join(ed.to);
  return out;
}

function buildDispatchSelfTestHarness() {
  // Strip the live-site assignment prefix. v1.0.23 uses `a=await` (unary) and `i=` (stream); keep this
  // tolerant of single-letter minified bindings. Free vars in the expression are r,e,o,t (and unary
  // native fallback also closes over hookContextCollector `i` — declare it in the harness so the
  // positive-control native path does not ReferenceError under "use strict").
  const unaryExpr = findEdit("unary tool dispatch -> CC").to.replace(/^\w+=await/, "");
  const streamExpr = findEdit("stream tool dispatch -> CC").to.replace(/^\w+=/, "").replace(/;for await$/, "");
  const harness =
    `\n;globalThis.__CC_SELFTEST_DISPATCH_U=function(r,e,o,t){var i=[];return Promise.resolve(${unaryExpr})};` +
    `globalThis.__CC_SELFTEST_DISPATCH_S=function(r,e,o,t){return ${streamExpr}};\n`;
  assertAscii("self-test harness", harness);
  return harness;
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

  const already = tryAlreadyPatchedShortCircuit(src);
  if (already) return already;

  const sha = verifyPristineBundleSha(src, env);
  messages.push(...sha.messages);

  for (const ed of edits) assertAscii(ed.name + ".to", ed.to);
  assertAnchorCounts(src);

  let out = replaceAnchors(src);
  out += buildDispatchSelfTestHarness();
  out = MARK + out;

  messages.push({
    level: "log",
    text: `patched @cursor/sdk@${version}: CC tool routing (unary+stream) + fromJson result construction`,
  });

  return { alreadyPatched: false, patchedSrc: out, observedSha: sha.observedSha, messages };
}

// Stamp the main entry so requiring @cursor/sdk evaluates the patched local-executor chunk immediately.
// Idempotent: skips when INDEX_EAGER_MARK is already present. Fails loud if the inject site is missing.
function stampIndexEntry(indexSrc) {
  if (typeof indexSrc !== "string") {
    throw new PatchError("bad-input", "stampIndexEntry requires the index source as a latin1 string");
  }
  if (indexSrc.includes(INDEX_EAGER_MARK)) {
    return { alreadyStamped: true, stampedSrc: indexSrc, messages: [{ level: "log", text: "index entry already eager-stamped" }] };
  }
  assertAscii("INDEX_EAGER_LOAD", INDEX_EAGER_LOAD);
  // webpack CJS entry ends with: })(),module.exports=o})();
  const needle = "module.exports=o})();";
  const idx = indexSrc.lastIndexOf(needle);
  if (idx < 0) {
    throw new PatchError(
      "index-inject-site-missing",
      `cannot find ${JSON.stringify(needle)} in dist/cjs/index.js — SDK entry shape changed; re-verify eager-load inject`,
    );
  }
  const stampedSrc = indexSrc.slice(0, idx) + INDEX_EAGER_LOAD + indexSrc.slice(idx);
  return {
    alreadyStamped: false,
    stampedSrc,
    messages: [{ level: "log", text: "stamped index.js with eager local-executor load (seams install on require)" }],
  };
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
  if (!result.alreadyPatched) {
    try {
      fs.writeFileSync(target, result.patchedSrc, "latin1");
    } catch (e) {
      return fail(`cannot write bundle: ${e.message}`);
    }
  }

  // Always ensure the main entry eagerly loads the patched chunk (idempotent).
  let indexSrc;
  try {
    indexSrc = fs.readFileSync(indexTarget, "latin1");
  } catch (e) {
    return fail(`cannot read index entry ${indexTarget}: ${e.message}`);
  }
  let stamped;
  try {
    stamped = stampIndexEntry(indexSrc);
  } catch (e) {
    if (e instanceof PatchError) return fail(e.message);
    return fail(`unexpected index stamp failure: ${e && e.message ? e.message : e}`);
  }
  emit(stamped.messages);
  if (!stamped.alreadyStamped) {
    try {
      fs.writeFileSync(indexTarget, stamped.stampedSrc, "latin1");
    } catch (e) {
      return fail(`cannot write index entry: ${e.message}`);
    }
  }

  process.exit(0);
}

if (require.main === module) {
  main();
}

module.exports = {
  applyPatch,
  stampIndexEntry,
  PatchError,
  PINNED_VERSION,
  EXPECTED_BUNDLE_SHA256,
  BUNDLE_REL,
  MARK,
  INDEX_EAGER_MARK,
  REQUIRED_SEAM_TOKENS,
  DISPATCH_EDIT_NAMES,
  SHA_OVERRIDE_ENV,
  edits,
};
