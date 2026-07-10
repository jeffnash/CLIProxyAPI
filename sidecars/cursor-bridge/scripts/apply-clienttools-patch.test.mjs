import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";
import { readFileSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const require = createRequire(import.meta.url);
const patcher = require("./apply-clienttools-patch.cjs");
const {
  applyPatch,
  validateDescriptor,
  discoverBundleSeams,
  findIndexSeams,
  PatchError,
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
} = patcher;

function bundleFixture({ names = ["U", "e", "t", "r", "n", "h"], duplicateUnary = false, omitAdvertise = false, omitArtifactPolicy = false, duplicateArtifactPolicy = false } = {}) {
  const [factory, caseArg, idArg, resultArg, messageVar, namespace] = names;
  const unary = `async function unary(r,e,o,t){const i=[],a=await r.exec.execute(e,o,{execId:t.execId,hookContextCollector:i});return a}`;
  return [
    `function ${factory}(${caseArg}){return function(${idArg},${resultArg}){const ${messageVar}={case:${caseArg},value:${resultArg}};return new ${namespace}.yT({id:${idArg},message:${messageVar}})}}`,
    unary,
    duplicateUnary ? unary.replace("unary", "unaryTwo") : "",
    `async function* stream(r,e,o,t){const i=r.exec.execute(e,o,{execId:t.execId});for await(const x of i)yield x}`,
    omitAdvertise ? "" : `const advertised=u.map((e=>({name:e.name,providerIdentifier:e.providerIdentifier,toolName:e.toolName,description:e.description,inputSchema:e.inputSchema?P.Value.fromJson(e.inputSchema):void 0})));`,
    omitArtifactPolicy ? "" : `const runtime={ignoreService:a,workspacePaths:[b],diagnosticsProvider:c,mcpMetaToolEnabled:d,createMcpStateAccessor:e,createSessionMcpLease:f,createSessionDependencies:g,mcpFileOutputThresholdBytes:void 0};`,
    duplicateArtifactPolicy ? `const runtimeTwo={ignoreService:a,workspacePaths:[b],diagnosticsProvider:c,mcpMetaToolEnabled:d,createMcpStateAccessor:e,createSessionMcpLease:f,createSessionDependencies:g,mcpFileOutputThresholdBytes:void 0};` : "",
  ].join(";");
}

function indexFixture({ omitLoader = false } = {}) {
  return `var a=function(){};${omitLoader ? "" : 'const loader=Promise.all([a.e(745),a.e(973)]).then(a.bind(a,"./src/agent/local-executor.ts"));'}var o={};module.exports=o;`;
}

function patchFixture(options = {}) {
  return applyPatch({
    src: bundleFixture(options),
    indexSrc: indexFixture(options),
    version: PINNED_VERSION,
    env: { [SHA_OVERRIDE_ENV[0]]: "1" },
  });
}

function throwsCode(fn, code) {
  assert.throws(fn, (error) => error instanceof PatchError && error.code === code);
}

test("the SDK pin covers package, local-executor chunk, and index hashes", () => {
  assert.equal(PINNED_VERSION, "1.0.23");
  assert.match(EXPECTED_BUNDLE_SHA256, /^[0-9a-f]{64}$/);
  assert.match(EXPECTED_INDEX_SHA256, /^[0-9a-f]{64}$/);
  assert.match(BUNDLE_REL.replaceAll("\\", "/"), /dist\/cjs\/973\.js$/);
  assert.match(INDEX_REL.replaceAll("\\", "/"), /dist\/cjs\/index\.js$/);
});

test("hash drift fails closed before structural discovery", () => {
  throwsCode(
    () => applyPatch({ src: "not javascript", indexSrc: indexFixture(), version: PINNED_VERSION }),
    "sha-mismatch",
  );
});

test("an override relaxes only hashes and emits explicit warnings", () => {
  const result = patchFixture();
  assert.equal(result.warnings.length, 2);
  assert.ok(result.warnings.every((warning) => warning.includes("development only")));
});

test("non-1 override values never relax the hash gate", () => {
  for (const value of ["true", "yes", "0", "01", ""]) {
    throwsCode(
      () => applyPatch({ src: bundleFixture(), indexSrc: indexFixture(), version: PINNED_VERSION, env: { [SHA_OVERRIDE_ENV[0]]: value } }),
      "sha-mismatch",
    );
  }
});

test("SDK version drift fails closed", () => {
  throwsCode(
    () => applyPatch({ src: bundleFixture(), indexSrc: indexFixture(), version: "1.0.24", env: { [SHA_OVERRIDE_ENV[0]]: "1" } }),
    "version-mismatch",
  );
});

test("AST discovery identifies each semantic seam exactly once", () => {
  const seams = discoverBundleSeams(bundleFixture());
  assert.equal(seams.serializer.functionName, "U");
  assert.equal(seams.unary.receiver, "r");
  assert.equal(seams.stream.receiver, "r");
  assert.equal(seams.advertise.receiverSource, "u");
  assert.equal(seams.artifactSpill.original, "void 0");
  assert.equal(seams.artifactSpill.metaToolOriginal, "d");
  const index = findIndexSeams(indexFixture());
  assert.deepEqual(index.loader.chunkIds, [745, 973]);
  assert.equal(index.loader.webpackRequire, "a");
});

test("AST discovery survives minifier identifier renames", () => {
  const result = patchFixture({ names: ["make", "kind", "wire", "payload", "message", "Proto"] });
  assert.match(result.patchedSrc, /globalThis\.__CC_SELFTEST_SERIALIZE=make/);
  assert.match(result.patchedSrc, /Proto\.yT\.fields\.list/);
  assert.match(result.patchedSrc, /globalThis\.__CC_EXEC_U/);
  assert.match(result.patchedSrc, /globalThis\.__CC_EXEC_S/);
});

test("a duplicate execution seam fails closed instead of patching both", () => {
  throwsCode(() => patchFixture({ duplicateUnary: true }), "structural-mismatch");
});

test("a missing advertise seam fails closed", () => {
  throwsCode(() => patchFixture({ omitAdvertise: true }), "structural-mismatch");
});

test("missing or duplicate SDK MCP artifact policy seams fail closed", () => {
  throwsCode(() => patchFixture({ omitArtifactPolicy: true }), "structural-mismatch");
  throwsCode(() => patchFixture({ duplicateArtifactPolicy: true }), "structural-mismatch");
});

test("a missing structural local-executor loader fails closed", () => {
  throwsCode(() => patchFixture({ omitLoader: true }), "structural-mismatch");
});

test("patched output installs direct tools while disabling native execution, artifact spill, and MCP meta wrappers", () => {
  const result = patchFixture();
  assert.ok(result.patchedSrc.startsWith(MARK));
  assert.ok(result.patchedIndexSrc.includes(INDEX_MARK));
  for (const capability of ["__CC_SELFTEST_SERIALIZE", "__CC_SELFTEST_DISPATCH_U", "__CC_SELFTEST_DISPATCH_S", "__CC_GET_ADVERTISE__"]) {
    assert.ok(result.patchedSrc.includes(capability), capability);
  }
  assert.match(result.patchedSrc, /native sidecar execution is forbidden/);
  assert.match(result.patchedSrc, /mcpFileOutputThresholdBytes:0/);
  assert.match(result.patchedSrc, /mcpMetaToolEnabled:false/);
  assert.match(result.patchedSrc, /__CC_GET_ADVERTISE__/);
});

test("the index eager-load is derived from the loader AST", () => {
  const result = patchFixture();
  assert.match(result.patchedIndexSrc, /a\.e\(745\);a\.e\(973\);a\("\.\/src\/agent\/local-executor\.ts"\)/);
});

test("descriptor records exact seam counts and patched hashes", () => {
  const result = patchFixture();
  assert.deepEqual(result.descriptor.seams, { ...EXPECTED_SEAMS });
  assert.equal(result.descriptor.nativeExecutionDefault, "deny");
  assert.equal(result.descriptor.mcpArtifactSpillThresholdBytes, 0);
  assert.equal(result.descriptor.mcpMetaToolEnabled, false);
  assert.equal(result.descriptor.sourceVerified, false);
  throwsCode(
    () => validateDescriptor({ descriptor: result.descriptor, version: PINNED_VERSION, bundleSource: result.patchedSrc, indexSource: result.patchedIndexSrc }),
    "descriptor-unverified",
  );
  assert.equal(
    validateDescriptor({ descriptor: result.descriptor, version: PINNED_VERSION, bundleSource: result.patchedSrc, indexSource: result.patchedIndexSrc, allowUnverified: true }),
    true,
  );
});

test("descriptor rejects byte tampering, seam tampering, and version skew", () => {
  const result = patchFixture();
  throwsCode(
    () => validateDescriptor({ descriptor: result.descriptor, version: PINNED_VERSION, bundleSource: `${result.patchedSrc} `, indexSource: result.patchedIndexSrc, allowUnverified: true }),
    "descriptor-hash-mismatch",
  );
  throwsCode(
    () => validateDescriptor({ descriptor: { ...result.descriptor, seams: { ...result.descriptor.seams, unaryDispatch: 0 } }, version: PINNED_VERSION, bundleSource: result.patchedSrc, indexSource: result.patchedIndexSrc }),
    "descriptor-invalid",
  );
  throwsCode(
    () => validateDescriptor({ descriptor: { ...result.descriptor, mcpMetaToolEnabled: true }, version: PINNED_VERSION, bundleSource: result.patchedSrc, indexSource: result.patchedIndexSrc, allowUnverified: true }),
    "descriptor-invalid",
  );
  throwsCode(
    () => validateDescriptor({ descriptor: result.descriptor, version: "1.0.24", bundleSource: result.patchedSrc, indexSource: result.patchedIndexSrc }),
    "version-mismatch",
  );
});

test("marked bytes without a descriptor cannot be blindly re-patched", () => {
  const result = patchFixture();
  throwsCode(
    () => applyPatch({ src: result.patchedSrc, indexSrc: result.patchedIndexSrc, version: PINNED_VERSION, env: { [SHA_OVERRIDE_ENV[0]]: "1" } }),
    "descriptor-required",
  );
});

test("the installed SDK descriptor verifies its actual vendor bytes", () => {
  const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..", "node_modules", "@cursor", "sdk");
  const descriptor = JSON.parse(readFileSync(path.join(root, DESCRIPTOR_REL), "utf8"));
  assert.equal(descriptor.sourceVerified, true);
  assert.equal(
    validateDescriptor({
      descriptor,
      version: JSON.parse(readFileSync(path.join(root, "package.json"), "utf8")).version,
      bundleSource: readFileSync(path.join(root, BUNDLE_REL), "latin1"),
      indexSource: readFileSync(path.join(root, INDEX_REL), "latin1"),
    }),
    true,
  );
});
