// Regression tests for the result-serialize self-test (ADD-74) in run-selftests.mjs.
//
// ADD-74: startup self-tests proved DISPATCH interception but NOT result serialization through the patched
// `$`/fromJson seam. run-selftests.mjs now drives representative read/write/shell/mcp result payloads through
// the live patched factory (captured as globalThis.__CC_SELFTEST_SERIALIZE by the patcher) and fails-fast if
// the seam cannot construct the SDK protobuf shape. These tests verify that selfTestResultSerialize:
//   - PASSES against a faithful seam (the four representative cases + both positive controls succeed),
//   - FAILS CLOSED when the capture global is missing (patch missing/stale),
//   - FAILS when the seam is a no-op passthrough (vacuity guard — proves the assertions aren't vacuous),
//   - FAILS when the seam does not VALIDATE shapes (the fromJson validation guard),
//   - FAILS when the seam returns the wrong oneof case.
//
// We drive the function against SYNTHETIC seams (a hand-written `$` that mimics the patched behavior), so the
// test never needs the real (gitignored, already-patched) @cursor/sdk bundle installed. Importing
// run-selftests.mjs must NOT trigger the live suite (it is guarded behind a run-as-main check); these tests
// assert that too.
//
// Run: node --test run-selftests.test.mjs

import { test } from "node:test";
import assert from "node:assert/strict";
import { selfTestResultSerialize } from "./run-selftests.mjs";

// The exact error strings the PATCHED `$` throws (apply-clienttools-patch.cjs). The runner's positive controls
// match on these, so a faithful synthetic seam must reproduce them verbatim (substring) — otherwise the test
// would be asserting against the wrong contract.
const UNKNOWN_CASE_ERR = (e) => `[clienttools] unknown result case ${e} (no ExecClientMessage field) -- sidecar mapping out of sync with SDK`;
const INVALID_SHAPE_ERR = (e, why) => `[clienttools] sidecar sent invalid result shape for ${e}: ${why}`;

// The ExecClientMessage oneof result cases the bridge actually emits (subset the self-test drives).
const KNOWN_CASES = new Set(["readResult", "writeResult", "shellResult", "mcpResult"]);

// A faithful synthetic `$`: mimics the patched factory closely enough to exercise the runner's contract.
// `opts` lets each test deliberately break ONE property to prove the corresponding guard bites.
function makeSeam(opts = {}) {
  const { acceptUnknownCase = false, skipShapeValidation = false, forceCase = null, returnWrapperValue = false } = opts;
  return function $(caseName) {
    return function (_id, n) {
      if (n && typeof n === "object" && "__ccJson" in n) {
        const j = n.__ccJson;
        if (!KNOWN_CASES.has(caseName) && !acceptUnknownCase) {
          throw new Error(UNKNOWN_CASE_ERR(caseName));
        }
        // Minimal shape validation: the result oneof must be `success` or `error`. A faithful fromJson rejects
        // a payload whose top-level key is neither (the real proto throws "cannot decode message ...").
        if (!skipShapeValidation) {
          const keys = Object.keys(j || {});
          const ok = keys.length > 0 && keys.every((k) => k === "success" || k === "error" || k === "rejected" || k === "allowlisted");
          if (!ok) {
            throw new Error(INVALID_SHAPE_ERR(caseName, "cannot decode message agent.v1." + caseName));
          }
        }
        // Build a "protobuf value" that is NOT the raw {__ccJson} wrapper (unless a test forces that break).
        const value = returnWrapperValue ? n : { __protoOf: caseName, decoded: j };
        return { id: _id, message: { case: forceCase || caseName, value } };
      }
      // Fast path (no __ccJson): pass the value through unchanged.
      return { id: _id, message: { case: caseName, value: n } };
    };
  };
}

function withSeam(seam, fn) {
  const saved = globalThis.__CC_SELFTEST_SERIALIZE;
  globalThis.__CC_SELFTEST_SERIALIZE = seam;
  try {
    return fn();
  } finally {
    globalThis.__CC_SELFTEST_SERIALIZE = saved;
  }
}

// Helper: run selfTestResultSerialize() and capture whether/why it rejected.
async function runSerialize(seam) {
  return withSeam(seam, async () => {
    try {
      await selfTestResultSerialize();
      return { ok: true };
    } catch (e) {
      return { ok: false, message: (e && e.message) || String(e) };
    }
  });
}

test("ADD-74: passes against a faithful serialize seam (4 cases + both positive controls)", async () => {
  const res = await runSerialize(makeSeam());
  assert.ok(res.ok, `faithful seam must pass; got: ${res.message}`);
});

test("ADD-74: fails CLOSED when the capture global is missing (patch missing/stale)", async () => {
  const res = await runSerialize(undefined);
  assert.equal(res.ok, false);
  assert.match(res.message, /did not install the result-serialize seam \(__CC_SELFTEST_SERIALIZE\)/);
  assert.match(res.message, /refusing to pass/);
});

test("ADD-74: fails when the capture global is not a function", async () => {
  const res = await runSerialize({ not: "a function" });
  assert.equal(res.ok, false);
  assert.match(res.message, /did not install the result-serialize seam/);
});

test("ADD-74 vacuity guard: fails when the seam never rejects an unknown case (no-op passthrough)", async () => {
  // A seam that accepts ANY case (never throws "unknown result case") would make the per-case assertions
  // meaningless — the runner must catch that the fromJson seam is effectively a passthrough.
  const res = await runSerialize(makeSeam({ acceptUnknownCase: true }));
  assert.equal(res.ok, false);
  assert.match(res.message, /did NOT reject an unknown result case/);
});

test("ADD-74 validation guard: fails when the seam does not validate result shapes", async () => {
  // A seam that builds a value for ANY payload (never throws "invalid result shape") means fromJson validation
  // is not engaged — a malformed sidecar result would be mis-decoded into a fabricated success.
  const res = await runSerialize(makeSeam({ skipShapeValidation: true }));
  assert.equal(res.ok, false);
  assert.match(res.message, /accepted a structurally invalid result shape/);
});

test("ADD-74: fails when the seam returns the wrong oneof case", async () => {
  const res = await runSerialize(makeSeam({ forceCase: "writeResult" }));
  assert.equal(res.ok, false);
  assert.match(res.message, /produced wrong oneof case/);
});

test("ADD-74: fails when the seam returns the raw __ccJson wrapper as the value (no fromJson)", async () => {
  // If the factory left value === the {__ccJson} wrapper, it never called fromJson; the runner must reject.
  const res = await runSerialize(makeSeam({ returnWrapperValue: true }));
  assert.equal(res.ok, false);
  assert.match(res.message, /did NOT build a protobuf value/);
});

test("ADD-74: a seam that throws on a representative case is surfaced as a fail-fast with the offending case", async () => {
  // Simulate fromJson throwing for one specific case (e.g. shellResult shape drift) — the runner must report
  // the case name and the underlying reason, not pass.
  const base = makeSeam();
  const throwingSeam = (caseName) => {
    if (caseName === "shellResult") {
      return () => {
        throw new Error("cannot decode message agent.v1.ShellResult: drift");
      };
    }
    return base(caseName);
  };
  const res = await runSerialize(throwingSeam);
  assert.equal(res.ok, false);
  assert.match(res.message, /FAILED to construct shellResult via fromJson/);
  assert.match(res.message, /refusing to pass/);
});

test("ADD-74: importing run-selftests.mjs does NOT run the live suite (guarded behind run-as-main)", async () => {
  // The import at the top of this file already happened without loading the SDK or printing the pass line.
  // Re-import to be explicit: it must resolve to the module exports without side effects (no throw, no exit).
  const mod = await import("./run-selftests.mjs");
  assert.equal(typeof mod.selfTestResultSerialize, "function", "selfTestResultSerialize must be exported for unit testing");
});
