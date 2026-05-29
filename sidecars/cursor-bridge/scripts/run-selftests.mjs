#!/usr/bin/env node
// Executes the bridge's fail-closed self-tests against the REAL patched @cursor/sdk bundle and exits
// nonzero on failure, so CI actually EXERCISES selfTestNativeUnreachable + selfTestBundleSeam + the ADD-74
// result-serialize seam (incl. their positive controls) rather than only grepping for the patch markers.
// Requires the patched bundle to be installed (npm ci runs the postinstall patcher). The bridge is imported
// dynamically inside runAll() and the suite only runs when this file is invoked directly (run-as-main guard
// at the bottom), so importing this module for unit tests neither loads the SDK nor starts the live suite.
// The live tests run sequentially (they share globals). RUN_AS_MAIN is false in the bridge here, so importing
// it does not start the server.
import { fileURLToPath } from "node:url";

// Run the full live self-test suite against the installed patched bundle. Kept as a function (not bare
// top-level) so the unit test can import selfTestResultSerialize() in isolation without triggering the
// bridge import + live SDK load (which require the patched bundle to be present).
async function runAll() {
  const bridge = await import("../cursor-agent-bridge.mjs");
  bridge.loadSdk(); // require + assert the patched bundle; installs __CC_SELFTEST_DISPATCH_U/S + __CC_SELFTEST_SERIALIZE
  await bridge.selfTestNativeUnreachable();
  await bridge.selfTestBundleSeam();
  // ADD-74: prove RESULT SERIALIZATION through the patched `$`/fromJson seam, not just dispatch interception.
  await selfTestResultSerialize();
  console.log("bridge self-tests passed (native unreachable + bundle seam + result serialize)");
}

// ADD-74: prove RESULT SERIALIZATION through the patched `$`/fromJson seam, not just dispatch interception.
// selfTestNativeUnreachable + selfTestBundleSeam only prove the patch intercepts the SDK's exec dispatch
// (native is unreachable). They do NOT prove that a returned `{__ccJson: ...}` result can be reconstructed
// into the SDK's ExecClientMessage protobuf via `<ResultType>.fromJson(...)` — the very path the FIRST real
// client tool result takes. A same-version bundle drift that renamed ExecClientMessage oneof fields or
// result message classes would still pass the dispatch self-tests, then fail on the first real tool result
// inside the patched serializer ("unknown result case" / "invalid result shape") AFTER startup announced
// success. This drives representative read/write/shell/mcp result payloads through the live patched `$`
// (captured onto globalThis.__CC_SELFTEST_SERIALIZE by the patcher) so that seam break is a FAIL-FAST deploy
// failure, never a runtime tool failure. CONTRACT: the patcher (scripts/apply-clienttools-patch.cjs) exposes
// the live `$` factory as globalThis.__CC_SELFTEST_SERIALIZE next to its definition; if that capture moves or
// is renamed, this self-test (and any bridge-side equivalent) must follow it.
//
// selfTestResultSerialize drives the EXACT result-construction path a real client tool result takes:
// `globalThis.__CC_SELFTEST_SERIALIZE(<resultCase>)(<execId>, { __ccJson: <innerOneofJSON> })`, which is the
// SDK's serializeResult/serializeStream factory `$` patched to run `ExecClientMessage.fields.find(...).T
// .fromJson(__ccJson)`. The payloads mirror the bridge's CC_CASES buildResult / MCP wrap outputs verbatim
// (the shapes the bridge actually emits over the seam), so a passing self-test reflects real traffic.
export async function selfTestResultSerialize() {
  const serialize = globalThis.__CC_SELFTEST_SERIALIZE;
  if (typeof serialize !== "function") {
    // The patch did not install the serializer-capture global. Either the bundle is unpatched/stale (npm ci +
    // postinstall did not run the current patcher) or the `$` capture was dropped. Fail CLOSED — never start /
    // pass CI claiming the seam is healthy when it was never exercised.
    throw new Error(
      "self-test: patched SDK bundle did not install the result-serialize seam (__CC_SELFTEST_SERIALIZE) — " +
        "patch missing/stale; reinstall a pristine bundle and re-run scripts/apply-clienttools-patch.cjs; refusing to pass",
    );
  }

  // Representative __ccJson result payloads, one per family the bridge can return over the seam. The inner
  // shape is the result message's oneof JSON (the bridge wraps it as { __ccJson: <this> }); `case` is the
  // ExecClientMessage oneof field the SDK passes to `$` for that tool family (readArgs -> "readResult", etc.).
  const cases = [
    {
      // CC_CASES.readArgs / redactedReadArgs buildResult success shape (cursor-agent-bridge.mjs).
      case: "readResult",
      payload: { success: { path: "/selftest/a.txt", content: "line1\nline2", totalLines: 2, fileSize: "11", truncated: false, rangeApplied: false } },
    },
    {
      // CC_CASES.writeArgs buildResult success shape.
      case: "writeResult",
      payload: { success: { path: "/selftest/a.txt", linesCreated: 2, fileSize: "11" } },
    },
    {
      // CC_CASES.shellArgs buildResult success shape (the shell "success" variant is the protocol's
      // failure channel too, via a non-zero exitCode — so exercising success covers both).
      case: "shellResult",
      payload: { success: { command: "echo hi", workingDirectory: "/workspace", exitCode: 0, stdout: "hi\n", stderr: "" } },
    },
    {
      // MCP dispatch wrap shape (handleMcp/mcpDispatch): McpResult.success.content is a list of typed parts.
      case: "mcpResult",
      payload: { success: { isError: false, content: [{ text: { text: "ok" } }] } },
    },
  ];

  for (const c of cases) {
    let msg;
    try {
      msg = serialize(c.case)("selftest-serialize-" + c.case, { __ccJson: c.payload });
    } catch (e) {
      // A throw here is the exact runtime symptom ADD-74 wants caught at startup: the seam cannot turn this
      // result into the SDK's protobuf shape. Surface it as a fail-fast with the offending case + reason.
      throw new Error(
        `self-test: result-serialize seam FAILED to construct ${c.case} via fromJson (${(e && e.message) || e}) ` +
          "— ExecClientMessage mapping likely drifted vs the SDK; refusing to pass",
      );
    }
    // The patched `$` returns `new I.yT({ id, message: { case, value } })` (ExecClientMessage). Assert the
    // oneof case round-tripped and a real (non-null/object) protobuf value was constructed — a degenerate
    // passthrough (e.g. value === the raw {__ccJson} wrapper, or a missing case) means the seam silently did
    // NOT call fromJson and would mis-serialize real traffic.
    const gotCase = msg && msg.message && msg.message.case;
    const gotValue = msg && msg.message && msg.message.value;
    if (gotCase !== c.case) {
      throw new Error(`self-test: result-serialize produced wrong oneof case for ${c.case} (got ${String(gotCase)}); refusing to pass`);
    }
    if (gotValue == null || typeof gotValue !== "object" || ("__ccJson" in gotValue)) {
      throw new Error(`self-test: result-serialize did NOT build a protobuf value for ${c.case} (seam passed the raw __ccJson wrapper through); refusing to pass`);
    }
  }

  // POSITIVE CONTROL 1 (vacuity guard): an UNKNOWN result case must throw the patch's "unknown result case"
  // guard. If it does not, the seam is a passthrough and the assertions above would be meaningless — exactly
  // the "self-test proves nothing" failure mode ADD-74 calls out. This is the SDK-mapping-out-of-sync signal.
  let unknownThrew = false;
  try {
    serialize("__cc_selftest_unknown_result_case__")("selftest-serialize-unknown", { __ccJson: { success: {} } });
  } catch (e) {
    unknownThrew = /unknown result case/.test((e && e.message) || "");
  }
  if (!unknownThrew) {
    throw new Error("self-test: result-serialize did NOT reject an unknown result case — the fromJson seam is a no-op passthrough (patch broken/stale); refusing to pass");
  }

  // POSITIVE CONTROL 2 (validation guard): a structurally INVALID payload for a known case must throw the
  // patch's "invalid result shape" guard (fromJson rejecting the bad shape). This proves the seam genuinely
  // VALIDATES the result against the SDK protobuf rather than blindly accepting whatever the sidecar sends —
  // so a malformed sidecar result fails loudly instead of being mis-decoded into a fabricated success.
  let invalidThrew = false;
  try {
    serialize("readResult")("selftest-serialize-invalid", { __ccJson: { __cc_selftest_not_a_real_oneof_key__: { nope: 1 } } });
  } catch (e) {
    invalidThrew = /invalid result shape/.test((e && e.message) || "");
  }
  if (!invalidThrew) {
    throw new Error("self-test: result-serialize accepted a structurally invalid result shape — fromJson validation is not engaged (patch broken/stale); refusing to pass");
  }
}

// Run as a CLI only when invoked directly (node scripts/run-selftests.mjs). Importing this module (e.g. from
// the unit test) does NOT load the SDK or start the live suite, so selfTestResultSerialize can be exercised
// against synthetic seams without a patched bundle installed.
if (process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]) {
  runAll().catch((e) => {
    console.error((e && e.message) || e);
    process.exit(1);
  });
}
