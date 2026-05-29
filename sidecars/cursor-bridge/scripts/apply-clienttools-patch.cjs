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
//   2. unary exec call -> `globalThis.__CC_EXEC_U` (falls back to native if unset).
//   3. stream exec call -> `globalThis.__CC_EXEC_S` (falls back to native if unset).
//
// If the globals are not installed (e.g. SDK used standalone), behavior is native.
//
// On any anchor mismatch or version drift the patcher fails LOUD (non-zero exit)
// so a new SDK release is re-verified before shipping.

const fs = require("fs");
const path = require("path");

const PINNED_VERSION = "1.0.14";
// SHA-256 of the pristine dist/cjs/index.js the anchors were verified against. On any SDK bump this
// changes — re-run anchor verification, then update PINNED_VERSION and this hash. The patcher logs the
// observed hash every run so a reviewer can confirm they patched the exact byte-stream.
const EXPECTED_BUNDLE_SHA256 = "fe49583a88f280b6efa32729dd724d10d5a07a04fcc1cdb16a75733678a8d7db";
const sdkRoot = path.join(__dirname, "..", "node_modules", "@cursor", "sdk");
const target = path.join(sdkRoot, "dist", "cjs", "index.js");
// Bump on any patch-shape change (e.g. adding the self-test harness) so an older-patched bundle is
// detected as stale: the anchor `from` strings won't match an already-patched bundle, so the patcher
// fails loud and the deploy must reinstall a pristine bundle (npm ci) before re-patching.
const MARK = "/*cursor-composer-clienttools-patched-v1*/";

function fail(msg) { console.error(`[clienttools] ${msg}`); process.exit(1); }

let version;
try { version = require(path.join(sdkRoot, "package.json")).version; }
catch (e) { fail(`cannot read @cursor/sdk package.json: ${e.message}`); }
if (version !== PINNED_VERSION) {
  fail(`@cursor/sdk is ${version} but patch is pinned to ${PINNED_VERSION}. ` +
       `Re-verify the anchors against the new bundle, then bump PINNED_VERSION.`);
}

let src;
try { src = fs.readFileSync(target, "latin1"); }
catch (e) { fail(`cannot read bundle ${target}: ${e.message}`); }

if (src.startsWith(MARK)) { console.log("[clienttools] already patched"); process.exit(0); }

const observedSha = require("crypto").createHash("sha256").update(src, "latin1").digest("hex");
if (observedSha !== EXPECTED_BUNDLE_SHA256) {
  console.warn(`[clienttools] WARNING: pristine bundle sha256=${observedSha} != recorded ${EXPECTED_BUNDLE_SHA256}. ` +
    `@cursor/sdk may have changed — re-verify anchors. Proceeding; the per-anchor count checks below still guard.`);
} else {
  console.log(`[clienttools] pristine bundle sha256 verified (${observedSha.slice(0, 16)}…)`);
}

const edits = [
  {
    name: "serializeResult/serializeStream fromJson",
    expect: 1,
    from: `function $(e){return function(t,n){const r={case:e,value:n};return new I.yT({id:t,message:r})}}`,
    to: `function $(e){return function(t,n){if(n&&typeof n==="object"&&"__ccJson"in n){var __j=n.__ccJson,__f=I.yT.fields.list().find(function(f){return f.localName===e||f.name===e});if(__f&&__f.T){try{n=__f.T.fromJson(__j)}catch(__err){throw new Error("[clienttools] sidecar sent invalid result shape for "+e+": "+((__err&&__err.message)||__err))}}else{throw new Error("[clienttools] unknown result case "+e+" (no ExecClientMessage field) -- sidecar mapping out of sync with SDK")}}var r={case:e,value:n};return new I.yT({id:t,message:r})}}`,
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
      fail(`injected string "${label}" has a non-ASCII char U+${str.charCodeAt(i).toString(16)} at index ${i}; the latin1 write would corrupt it -- use ASCII.`);
    }
  }
}
for (const ed of edits) assertAscii(ed.name + ".to", ed.to);

for (const ed of edits) {
  const count = src.split(ed.from).length - 1;
  if (count !== ed.expect) {
    fail(`anchor "${ed.name}": found ${count}, expected ${ed.expect}. SDK bundle changed — re-verify.`);
  }
}
for (const ed of edits) src = src.split(ed.from).join(ed.to);

// Bundle-level self-test harness: expose the EXACT unary/stream seam expressions (the same strings just
// applied at the real dispatch sites) as globals, so the bridge can drive the real patched dispatch logic
// at startup and prove native exec.execute() is unreachable. Derived from the same `to` strings, so the
// harness can never drift from the live seam; the anchor-count checks above guarantee the live seam exists.
const unaryExpr = edits.find((e) => e.name === "unary tool dispatch -> CC").to.replace(/^o=await/, "");
const streamExpr = edits.find((e) => e.name === "stream tool dispatch -> CC").to.replace(/^o=/, "").replace(/;for await$/, "");
const harness = `\n;globalThis.__CC_SELFTEST_DISPATCH_U=function(n,e,s,t){return Promise.resolve(${unaryExpr})};` +
  `globalThis.__CC_SELFTEST_DISPATCH_S=function(n,e,s,t){return ${streamExpr}};\n`;
assertAscii("self-test harness", harness);
src += harness;

src = MARK + src;

try { fs.writeFileSync(target, src, "latin1"); }
catch (e) { fail(`cannot write bundle: ${e.message}`); }
console.log(`[clienttools] patched @cursor/sdk@${version}: CC tool routing (unary+stream) + fromJson result construction`);
