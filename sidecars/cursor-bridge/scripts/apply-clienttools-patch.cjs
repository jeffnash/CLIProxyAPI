#!/usr/bin/env node
// Structural, version-pinned patcher for @cursor/sdk's client-tool boundary.
//
// The patch remains necessary because the public SDK agent API does not expose a
// serializable pause/resume callback for client-owned tools. Keep the patch as
// small as possible: intercept unary/stream execution, construct protobuf result
// messages, and append the bridge's single advertised-tool registry. Discovery is
// by AST shape, never by a minified byte-string anchor. Exact package and pristine
// hashes remain fail-closed so structural similarity alone cannot bless an SDK
// upgrade.

"use strict";

const fs = require("fs");
const path = require("path");
const crypto = require("crypto");
const acorn = require("acorn");
const walk = require("acorn-walk");

const PATCHER_VERSION = 4;
const DESCRIPTOR_VERSION = 3;
const PINNED_VERSION = "1.0.23";
const EXPECTED_BUNDLE_SHA256 = "829ced604bb88908e49fcf5cd31eb22bce4e57d32074b2846d86a6c5afa26881";
const EXPECTED_INDEX_SHA256 = "3157e86833e5033ce7b870cfd9810edc4b1e9c0637b93170779d6cbb3feba022";
const BUNDLE_REL = path.join("dist", "cjs", "973.js");
const INDEX_REL = path.join("dist", "cjs", "index.js");
const DESCRIPTOR_REL = ".clienttools-patch-descriptor.json";
const MARK = "/*cursor-composer-clienttools-patched-v4*/";
const INDEX_MARK = "/*cursor-composer-clienttools-eager-v4*/";
const MODULE_ID = "./src/agent/local-executor.ts";
const SHA_OVERRIDE_ENV = ["CURSOR_CLIENTTOOLS_ALLOW_UNVERIFIED_SDK", "CURSOR_SDK_PATCH_ALLOW_SHA_MISMATCH"];
const EXPECTED_SEAMS = Object.freeze({
  serializer: 1,
  unaryDispatch: 1,
  streamDispatch: 1,
  advertiseRegistry: 1,
  mcpArtifactSpillPolicy: 1,
  mcpMetaToolPolicy: 1,
  localExecutorLoader: 1,
  moduleExport: 1,
});

const sdkRoot = path.join(__dirname, "..", "node_modules", "@cursor", "sdk");
const target = path.join(sdkRoot, BUNDLE_REL);
const indexTarget = path.join(sdkRoot, INDEX_REL);
const descriptorTarget = path.join(sdkRoot, DESCRIPTOR_REL);

class PatchError extends Error {
  constructor(code, message) {
    super(message);
    this.name = "PatchError";
    this.code = code;
  }
}

function sha256(value, encoding = undefined) {
  return crypto.createHash("sha256").update(value, encoding).digest("hex");
}

function parse(source, label) {
  try {
    return acorn.parse(source, { ecmaVersion: "latest", sourceType: "script", allowHashBang: true });
  } catch (error) {
    throw new PatchError("parse-failed", `${label} is not valid JavaScript: ${error.message}`);
  }
}

function propName(node) {
  if (!node) return "";
  if (!node.computed && node.key?.type === "Identifier") return node.key.name;
  if (node.key?.type === "Literal") return String(node.key.value);
  return "";
}

function memberName(node) {
  if (node?.type !== "MemberExpression") return "";
  if (!node.computed && node.property.type === "Identifier") return node.property.name;
  if (node.computed && node.property.type === "Literal") return String(node.property.value);
  return "";
}

function sourceOf(source, node) {
  return source.slice(node.start, node.end);
}

function one(candidates, name) {
  if (candidates.length !== 1) {
    throw new PatchError(
      "structural-mismatch",
      `@cursor/sdk ${name} seam: found ${candidates.length}, expected exactly 1; refusing to patch`,
    );
  }
  return candidates[0];
}

function isIdentifier(node, name) {
  return node?.type === "Identifier" && node.name === name;
}

function findSerializer(source, ast) {
  const candidates = [];
  walk.full(ast, (outer) => {
    if (outer.type !== "FunctionDeclaration" || !outer.id || outer.params.length !== 1) return;
    const statements = outer.body?.body || [];
    if (statements.length !== 1 || statements[0].type !== "ReturnStatement") return;
    const inner = statements[0].argument;
    if (inner?.type !== "FunctionExpression" || inner.params.length !== 2) return;
    const innerStatements = inner.body?.body || [];
    if (innerStatements.length !== 2 || innerStatements[0].type !== "VariableDeclaration" || innerStatements[1].type !== "ReturnStatement") return;
    const declarations = innerStatements[0].declarations || [];
    if (declarations.length !== 1 || declarations[0].id.type !== "Identifier" || declarations[0].init?.type !== "ObjectExpression") return;
    const messageVar = declarations[0].id.name;
    const messageProps = new Map(declarations[0].init.properties.map((p) => [propName(p), p.value]));
    if (!isIdentifier(messageProps.get("case"), outer.params[0].name) || !isIdentifier(messageProps.get("value"), inner.params[1].name)) return;
    const returned = innerStatements[1].argument;
    if (returned?.type !== "NewExpression" || returned.arguments.length !== 1 || returned.arguments[0].type !== "ObjectExpression") return;
    const resultProps = new Map(returned.arguments[0].properties.map((p) => [propName(p), p.value]));
    if (!isIdentifier(resultProps.get("id"), inner.params[0].name) || !isIdentifier(resultProps.get("message"), messageVar)) return;
    candidates.push({
      outer,
      inner,
      caseParam: outer.params[0].name,
      resultParam: inner.params[1].name,
      constructor: sourceOf(source, returned.callee),
      functionName: outer.id.name,
    });
  });
  return one(candidates, "serializer");
}

function isExecCall(node) {
  return (
    node?.type === "CallExpression" &&
    memberName(node.callee) === "execute" &&
    memberName(node.callee.object) === "exec" &&
    node.arguments.length === 3 &&
    node.arguments[2].type === "ObjectExpression" &&
    node.arguments[2].properties.some((p) => propName(p) === "execId")
  );
}

function findDispatches(source, ast) {
  const unary = [];
  const stream = [];
  walk.full(ast, (node) => {
    if (!isExecCall(node)) return;
    const optionProps = new Map(node.arguments[2].properties.map((p) => [propName(p), p.value]));
    const execId = optionProps.get("execId");
    if (execId?.type !== "MemberExpression" || memberName(execId) !== "execId") return;
    const receiver = node.callee.object.object;
    if (receiver?.type !== "Identifier" || node.arguments[0].type !== "Identifier" || node.arguments[1].type !== "Identifier" || execId.object.type !== "Identifier") return;
    const item = {
      node,
      original: sourceOf(source, node),
      receiver: sourceOf(source, receiver),
      tool: sourceOf(source, node.arguments[0]),
      input: sourceOf(source, node.arguments[1]),
      envelope: sourceOf(source, execId.object),
    };
    if (optionProps.has("hookContextCollector")) unary.push(item);
    else stream.push(item);
  });
  return { unary: one(unary, "unary dispatch"), stream: one(stream, "stream dispatch") };
}

function findAdvertiseRegistry(source, ast) {
  const candidates = [];
  walk.full(ast, (node) => {
    if (node.type !== "CallExpression" || memberName(node.callee) !== "map" || node.arguments.length !== 1) return;
    const callback = node.arguments[0];
    const body = callback.type === "ArrowFunctionExpression" ? callback.body : null;
    if (body?.type !== "ObjectExpression") return;
    const keys = new Set(body.properties.map(propName));
    for (const required of ["name", "providerIdentifier", "toolName", "description", "inputSchema"]) {
      if (!keys.has(required)) return;
    }
    const receiver = node.callee.object;
    if (receiver.type !== "Identifier") return;
    candidates.push({ receiver, receiverSource: sourceOf(source, receiver) });
  });
  return one(candidates, "advertised-tool registry");
}

function findMcpArtifactSpillPolicy(source, ast) {
  const candidates = [];
  walk.fullAncestor(ast, (node, ancestors) => {
    if (node.type !== "Property" || propName(node) !== "mcpFileOutputThresholdBytes") return;
    if (
      node.value?.type !== "UnaryExpression" ||
      node.value.operator !== "void" ||
      node.value.argument?.type !== "Literal" ||
      node.value.argument.value !== 0
    ) return;
    const owner = [...ancestors].reverse().find(
      (candidate) => candidate.type === "ObjectExpression" && candidate.properties.includes(node),
    );
    if (!owner) return;
    const keys = new Set(owner.properties.map(propName));
    for (const required of [
      "ignoreService",
      "workspacePaths",
      "diagnosticsProvider",
      "mcpMetaToolEnabled",
      "createMcpStateAccessor",
      "createSessionMcpLease",
      "createSessionDependencies",
    ]) {
      if (!keys.has(required)) return;
    }
    const metaToolNode = owner.properties.find((property) => propName(property) === "mcpMetaToolEnabled");
    if (!metaToolNode || !metaToolNode.value) return;
    candidates.push({
      node,
      value: node.value,
      owner,
      original: sourceOf(source, node.value),
      metaToolNode,
      metaToolValue: metaToolNode.value,
      metaToolOriginal: sourceOf(source, metaToolNode.value),
    });
  });
  return one(candidates, "MCP artifact-spill policy");
}

function discoverBundleSeams(source) {
  const ast = parse(source, BUNDLE_REL);
  return {
    serializer: findSerializer(source, ast),
    ...findDispatches(source, ast),
    advertise: findAdvertiseRegistry(source, ast),
    artifactSpill: findMcpArtifactSpillPolicy(source, ast),
  };
}

function findIndexSeams(source) {
  const ast = parse(source, INDEX_REL);
  const loaders = [];
  const exports = [];
  walk.ancestor(ast, {
    CallExpression(node, ancestors) {
      if (memberName(node.callee) !== "bind" || node.arguments.length < 2) return;
      if (!node.arguments.some((arg) => arg.type === "Literal" && arg.value === MODULE_ID)) return;
      const thenCall = ancestors.slice(0, -1).reverse().find((candidate) => candidate.type === "CallExpression" && candidate.arguments.includes(node));
      const allCall = thenCall?.callee?.object;
      if (memberName(thenCall?.callee) !== "then" || allCall?.type !== "CallExpression" || memberName(allCall.callee) !== "all") return;
      const entries = allCall.arguments[0];
      if (entries?.type !== "ArrayExpression" || node.callee.object.type !== "Identifier") return;
      const webpackRequire = node.callee.object.name;
      const chunkIds = [];
      for (const entry of entries.elements) {
        if (
          entry?.type !== "CallExpression" ||
          memberName(entry.callee) !== "e" ||
          !isIdentifier(entry.callee.object, webpackRequire) ||
          entry.arguments.length !== 1 ||
          entry.arguments[0].type !== "Literal" ||
          !Number.isInteger(entry.arguments[0].value)
        ) return;
        chunkIds.push(entry.arguments[0].value);
      }
      if (!chunkIds.length) return;
      loaders.push({ webpackRequire, chunkIds });
    },
    AssignmentExpression(node) {
      if (
        node.operator === "=" &&
        node.left?.type === "MemberExpression" &&
        !node.left.computed &&
        isIdentifier(node.left.object, "module") &&
        isIdentifier(node.left.property, "exports")
      ) exports.push(node);
    },
  });
  return { loader: one(loaders, "local-executor loader"), moduleExport: one(exports, "module export") };
}

function applyEdits(source, edits) {
  const ordered = [...edits].sort((a, b) => b.start - a.start || b.end - a.end);
  let lastStart = source.length + 1;
  let output = source;
  for (const edit of ordered) {
    if (edit.end > lastStart) throw new PatchError("overlapping-edits", `structural edits overlap at ${edit.start}:${edit.end}`);
    output = output.slice(0, edit.start) + edit.text + output.slice(edit.end);
    lastStart = edit.start;
  }
  return output;
}

function shaOverrideEnabled(env) {
  return SHA_OVERRIDE_ENV.some((key) => env[key] === "1");
}

function verifyHash({ source, expected, label, env }) {
  const observed = sha256(source, "latin1");
  if (observed === expected) return { observed, warning: null };
  const activeOverride = SHA_OVERRIDE_ENV.find((key) => env[key] === "1");
  if (!shaOverrideEnabled(env)) {
    throw new PatchError(
      "sha-mismatch",
      `${label} sha256=${observed} does not match pinned ${expected}; reinstall @cursor/sdk@${PINNED_VERSION} or review the new SDK structurally before updating the pin`,
    );
  }
  return { observed, warning: `${activeOverride}=1 accepted an unverified ${label} sha256=${observed}; development only` };
}

function assertVersion(version) {
  if (version !== PINNED_VERSION) {
    throw new PatchError("version-mismatch", `@cursor/sdk is ${version}; client-tools patch is pinned to ${PINNED_VERSION}`);
  }
}

function dispatchReplacement(kind, dispatch) {
  const globalName = kind === "unary" ? "__CC_EXEC_U" : "__CC_EXEC_S";
  const refused = kind === "unary"
    ? 'Promise.reject(new Error("[clienttools] bridge callback missing; native sidecar execution is forbidden"))'
    : '(async function*(){throw new Error("[clienttools] bridge callback missing; native sidecar execution is forbidden")})()';
  return `(globalThis.${globalName}?globalThis.${globalName}(${dispatch.receiver},${dispatch.tool},${dispatch.input},${dispatch.envelope}):(globalThis.__CC_ALLOW_NATIVE?${dispatch.original}:${refused}))`;
}

function serializerInjection(seam) {
  const r = seam.resultParam;
  const c = seam.caseParam;
  const ctor = seam.constructor;
  return `if(${r}&&typeof ${r}==="object"&&"__ccJson" in ${r}){var __j=${r}.__ccJson,__f=${ctor}.fields.list().find(function(__field){return __field.localName===${c}||__field.name===${c}});if(!__f||!__f.T)throw new Error("[clienttools] unknown result case "+${c});try{${r}=__f.T.fromJson(__j)}catch(__error){throw new Error("[clienttools] invalid result shape for "+${c}+": "+((__error&&__error.message)||__error))}}`;
}

function dispatchHarness() {
  return (
    `;globalThis.__CC_SELFTEST_DISPATCH_U=function(__r,__tool,__input,__meta){var __hooks=[];return Promise.resolve(` +
    `(globalThis.__CC_EXEC_U?globalThis.__CC_EXEC_U(__r,__tool,__input,__meta):(globalThis.__CC_ALLOW_NATIVE?__r.exec.execute(__tool,__input,{execId:__meta.execId,hookContextCollector:__hooks}):Promise.reject(new Error("[clienttools] bridge callback missing; native sidecar execution is forbidden")))))};` +
    `globalThis.__CC_SELFTEST_DISPATCH_S=function(__r,__tool,__input,__meta){return ` +
    `(globalThis.__CC_EXEC_S?globalThis.__CC_EXEC_S(__r,__tool,__input,__meta):(globalThis.__CC_ALLOW_NATIVE?__r.exec.execute(__tool,__input,{execId:__meta.execId}):(async function*(){throw new Error("[clienttools] bridge callback missing; native sidecar execution is forbidden")})()))};`
  );
}

function applyPatch({ src, indexSrc, version, env = {} }) {
  if (typeof src !== "string" || typeof indexSrc !== "string") {
    throw new PatchError("bad-input", "applyPatch requires latin1 bundle and index source strings");
  }
  assertVersion(version);
  if (src.startsWith(MARK) || indexSrc.includes(INDEX_MARK)) {
    throw new PatchError("descriptor-required", "patched SDK bytes were found without a verified descriptor; run npm ci before patching again");
  }
  const bundleHash = verifyHash({ source: src, expected: EXPECTED_BUNDLE_SHA256, label: BUNDLE_REL, env });
  const indexHash = verifyHash({ source: indexSrc, expected: EXPECTED_INDEX_SHA256, label: INDEX_REL, env });
  const seams = discoverBundleSeams(src);
  const indexSeams = findIndexSeams(indexSrc);

  const receiver = seams.advertise.receiverSource;
  const advertise = `(Array.isArray(${receiver})?${receiver}.slice():[]).concat(globalThis.__CC_GET_ADVERTISE__?globalThis.__CC_GET_ADVERTISE__():[])`;
  let patchedSrc = applyEdits(src, [
    { start: seams.serializer.inner.body.start + 1, end: seams.serializer.inner.body.start + 1, text: serializerInjection(seams.serializer) },
    { start: seams.serializer.outer.end, end: seams.serializer.outer.end, text: `;try{globalThis.__CC_SELFTEST_SERIALIZE=${seams.serializer.functionName}}catch(__error){}` },
    { start: seams.unary.node.start, end: seams.unary.node.end, text: dispatchReplacement("unary", seams.unary) },
    { start: seams.stream.node.start, end: seams.stream.node.end, text: dispatchReplacement("stream", seams.stream) },
    { start: seams.advertise.receiver.start, end: seams.advertise.receiver.end, text: advertise },
    // The SDK otherwise writes MCP results over 40,000 bytes to
    // <projectDir>/agent-tools/<uuid>.txt. A remote client-tools bridge has no
    // valid proxy-side workspace, so artifact materialization must stay off:
    // the full result remains in the protocol and any real filesystem action
    // continues through the harness-owned tool callback.
    { start: seams.artifactSpill.value.start, end: seams.artifactSpill.value.end, text: "0" },
    // Keep direct requestContext.tools advertisement, but disable Cursor's
    // generic GetMcpTools/CallMcpTool meta wrappers at the executor source.
    // This property is in the same structurally asserted runtime policy as the
    // artifact threshold, so SDK drift fails closed rather than re-enabling it.
    { start: seams.artifactSpill.metaToolValue.start, end: seams.artifactSpill.metaToolValue.end, text: "false" },
  ]);
  patchedSrc = MARK + patchedSrc + dispatchHarness();
  parse(patchedSrc, `patched ${BUNDLE_REL}`);

  const req = indexSeams.loader.webpackRequire;
  const eager = `${INDEX_MARK}(()=>{try{${indexSeams.loader.chunkIds.map((id) => `${req}.e(${id})`).join(";")};${req}(${JSON.stringify(MODULE_ID)})}catch(__error){}})(),`;
  const patchedIndexSrc = applyEdits(indexSrc, [
    { start: indexSeams.moduleExport.start, end: indexSeams.moduleExport.start, text: eager },
  ]);
  parse(patchedIndexSrc, `patched ${INDEX_REL}`);

  const warnings = [bundleHash.warning, indexHash.warning].filter(Boolean);
  const descriptor = {
    descriptorVersion: DESCRIPTOR_VERSION,
    patcherVersion: PATCHER_VERSION,
    sdkVersion: version,
    bundle: BUNDLE_REL.replaceAll(path.sep, "/"),
    index: INDEX_REL.replaceAll(path.sep, "/"),
    pristineBundleSha256: bundleHash.observed,
    pristineIndexSha256: indexHash.observed,
    patchedBundleSha256: sha256(patchedSrc, "latin1"),
    patchedIndexSha256: sha256(patchedIndexSrc, "latin1"),
    seams: { ...EXPECTED_SEAMS },
    nativeExecutionDefault: "deny",
    mcpArtifactSpillThresholdBytes: 0,
    mcpMetaToolEnabled: false,
    sourceVerified: warnings.length === 0,
  };
  return { patchedSrc, patchedIndexSrc, descriptor, warnings };
}

function validateDescriptor({ descriptor, version, bundleSource, indexSource, allowUnverified = false }) {
  if (!descriptor || typeof descriptor !== "object") throw new PatchError("descriptor-invalid", "patch descriptor is not an object");
  if (descriptor.descriptorVersion !== DESCRIPTOR_VERSION || descriptor.patcherVersion !== PATCHER_VERSION) {
    throw new PatchError("descriptor-invalid", "patch descriptor version is stale");
  }
  assertVersion(version);
  if (descriptor.sdkVersion !== version || descriptor.bundle !== BUNDLE_REL.replaceAll(path.sep, "/") || descriptor.index !== INDEX_REL.replaceAll(path.sep, "/")) {
    throw new PatchError("descriptor-invalid", "patch descriptor does not describe the installed SDK files");
  }
  if (
    JSON.stringify(descriptor.seams) !== JSON.stringify(EXPECTED_SEAMS) ||
    descriptor.nativeExecutionDefault !== "deny" ||
    descriptor.mcpArtifactSpillThresholdBytes !== 0 ||
    descriptor.mcpMetaToolEnabled !== false
  ) {
    throw new PatchError("descriptor-invalid", "patch descriptor seam contract is incomplete");
  }
  const verifiedSource = descriptor.sourceVerified === true &&
    descriptor.pristineBundleSha256 === EXPECTED_BUNDLE_SHA256 &&
    descriptor.pristineIndexSha256 === EXPECTED_INDEX_SHA256;
  if (!verifiedSource && !allowUnverified) {
    throw new PatchError("descriptor-unverified", "patch descriptor was not produced from the exact pinned pristine SDK bytes");
  }
  if (descriptor.patchedBundleSha256 !== sha256(bundleSource, "latin1") || descriptor.patchedIndexSha256 !== sha256(indexSource, "latin1")) {
    throw new PatchError("descriptor-hash-mismatch", "installed SDK bytes do not match the signed-off patch descriptor");
  }
  if (!bundleSource.startsWith(MARK) || !indexSource.includes(INDEX_MARK)) {
    throw new PatchError("descriptor-invalid", "descriptor matches files that do not carry the structural patch markers");
  }
  parse(bundleSource, `installed ${BUNDLE_REL}`);
  parse(indexSource, `installed ${INDEX_REL}`);
  return true;
}

function atomicWrite(file, data, encoding = undefined) {
  const temp = `${file}.tmp-${process.pid}-${crypto.randomBytes(6).toString("hex")}`;
  let fd;
  try {
    fd = fs.openSync(temp, "wx", 0o600);
    fs.writeFileSync(fd, data, encoding);
    fs.fsyncSync(fd);
    fs.closeSync(fd);
    fd = undefined;
    fs.renameSync(temp, file);
    const dir = fs.openSync(path.dirname(file), "r");
    try { fs.fsyncSync(dir); } finally { fs.closeSync(dir); }
  } finally {
    if (fd !== undefined) fs.closeSync(fd);
    try { fs.unlinkSync(temp); } catch (error) { if (error.code !== "ENOENT") throw error; }
  }
}

function existingInstallIsValid(version) {
  if (!fs.existsSync(descriptorTarget)) return false;
  let descriptor;
  try {
    descriptor = JSON.parse(fs.readFileSync(descriptorTarget, "utf8"));
    validateDescriptor({
      descriptor,
      version,
      bundleSource: fs.readFileSync(target, "latin1"),
      indexSource: fs.readFileSync(indexTarget, "latin1"),
    });
    return true;
  } catch (error) {
    throw new PatchError("descriptor-invalid", `installed client-tools patch failed descriptor validation: ${error.message}; run npm ci`);
  }
}

function main() {
  try {
    const version = JSON.parse(fs.readFileSync(path.join(sdkRoot, "package.json"), "utf8")).version;
    assertVersion(version);
    if (existingInstallIsValid(version)) {
      console.log(`[clienttools] verified existing structural patch for @cursor/sdk@${version}`);
      return;
    }
    const result = applyPatch({
      src: fs.readFileSync(target, "latin1"),
      indexSrc: fs.readFileSync(indexTarget, "latin1"),
      version,
      env: process.env,
    });
    for (const warning of result.warnings) console.warn(`[clienttools] WARNING: ${warning}`);
    atomicWrite(target, result.patchedSrc, "latin1");
    atomicWrite(indexTarget, result.patchedIndexSrc, "latin1");
    atomicWrite(descriptorTarget, `${JSON.stringify(result.descriptor, null, 2)}\n`, "utf8");
    validateDescriptor({
      descriptor: result.descriptor,
      version,
      bundleSource: result.patchedSrc,
      indexSource: result.patchedIndexSrc,
      allowUnverified: result.warnings.length > 0,
    });
    console.log(`[clienttools] structurally patched @cursor/sdk@${version}; descriptor=${descriptorTarget}`);
  } catch (error) {
    const message = error instanceof PatchError ? `${error.code}: ${error.message}` : `unexpected: ${error.stack || error}`;
    console.error(`[clienttools] ${message}`);
    process.exitCode = 1;
  }
}

if (require.main === module) main();

module.exports = {
  applyPatch,
  validateDescriptor,
  discoverBundleSeams,
  findMcpArtifactSpillPolicy,
  findIndexSeams,
  PatchError,
  PATCHER_VERSION,
  DESCRIPTOR_VERSION,
  PINNED_VERSION,
  EXPECTED_BUNDLE_SHA256,
  EXPECTED_INDEX_SHA256,
  EXPECTED_SEAMS,
  BUNDLE_REL,
  INDEX_REL,
  DESCRIPTOR_REL,
  MARK,
  INDEX_MARK,
  SHA_OVERRIDE_ENV,
};
