// Unit tests for the pure helpers in cursor-agent-bridge.mjs. Run with: node --test
// Importing the bridge does NOT load @cursor/sdk (loadSdk is lazy + RUN_AS_MAIN is false here), so these
// run without the SDK's native deps.
import test from "node:test";
import assert from "node:assert/strict";
import {
  reconcileExport,
  toSdkImages,
  constraintInstructions,
  effectiveAdvertise,
  parseShellContent,
  headlessRequestContext,
  Session,
  ccToolId,
  authorizeRequestWith,
  platformHasSession,
  keyHash,
} from "./cursor-agent-bridge.mjs";

test("reconcileToolName: exact / case-insensitive / single / token-boundary / ambiguous", () => {
  const adv = [{ name: "Read" }, { name: "Bash" }, { name: "reconfigure_database" }];
  assert.equal(reconcileExport(adv, "Read"), "Read"); // exact
  assert.equal(reconcileExport(adv, "read"), "Read"); // case-insensitive unique
  assert.equal(reconcileExport([{ name: "OnlyTool" }], "anything"), "OnlyTool"); // single tool
  // NO substring misroute: "config" must NOT match "reconfigure_database" (the historical bug).
  assert.equal(reconcileExport(adv, "config"), null);
  // token-boundary unique match.
  assert.equal(reconcileExport([{ name: "search_files" }, { name: "Bash" }], "files"), "search_files");
  // ambiguous tail -> null.
  assert.equal(reconcileExport([{ name: "read_file" }, { name: "write_file" }], "file"), null);
});

test("toSdkImages preserves mimeType and validates both fields (Comment 2)", () => {
  const out = toSdkImages([{ data: "QUJD", mimeType: "image/png" }]);
  assert.deepEqual(out, [{ data: "QUJD", mimeType: "image/png" }]);
  assert.equal(toSdkImages(undefined).length, 0);
  // Missing mimeType / data must throw (the SDK requires both for inline image data).
  assert.throws(() => toSdkImages([{ data: "QUJD" }]), /mimeType/);
  assert.throws(() => toSdkImages([{ mimeType: "image/png" }]), /data\/mimeType/);
  assert.throws(() => toSdkImages([{ data: "", mimeType: "image/png" }]), /data\/mimeType/);
});

test("constraintInstructions: tool_choice required / specific (Comment 3)", () => {
  assert.match(constraintInstructions({ toolChoice: "required" }), /MUST call one of the available tools/);
  const sp = constraintInstructions({ toolChoice: "specific:Bash" });
  assert.match(sp, /MUST call the tool named "Bash"/);
  assert.match(sp, /only that tool/);
  // none/auto/unset add no tool instruction.
  assert.equal(constraintInstructions({ toolChoice: "none" }), "");
  assert.equal(constraintInstructions({ toolChoice: "auto" }), "");
  assert.equal(constraintInstructions({}), "");
});

test("constraintInstructions: response_format json_object / json_schema (Comment 3)", () => {
  assert.match(constraintInstructions({ responseFormat: { type: "json_object" } }), /single valid JSON object only/);
  const schema = { type: "object", properties: { a: { type: "string" } } };
  const out = constraintInstructions({ responseFormat: { type: "json_schema", json_schema: { name: "x", schema } } });
  assert.match(out, /conforms EXACTLY to this JSON Schema/);
  assert.ok(out.includes(JSON.stringify(schema)), "schema JSON should be embedded in the instruction");
});

test("constraintInstructions: stop sequences + token limit (Comment 3)", () => {
  const out = constraintInstructions({ stop: ["END", "STOP"], maxTokens: 200 });
  assert.match(out, /Stop your response immediately before emitting any of these sequences: "END", "STOP"/);
  assert.match(out, /within approximately 200 tokens/);
});

test("effectiveAdvertise gates the visible tool set by tool_choice (Comment 3)", () => {
  const adv = [{ name: "Read" }, { name: "Bash" }];
  assert.deepEqual(effectiveAdvertise(adv, "none"), []); // none -> hide all
  assert.deepEqual(effectiveAdvertise(adv, "specific:Bash"), [{ name: "Bash" }]); // specific -> just that one
  assert.deepEqual(effectiveAdvertise(adv, "auto"), adv); // auto -> full set
  assert.deepEqual(effectiveAdvertise(adv, ""), adv); // unset -> full set
  // specific:<unknown> falls back to the full set (better than advertising nothing).
  assert.deepEqual(effectiveAdvertise(adv, "specific:Nope"), adv);
});

test("parseShellContent: structured object, JSON string, and plain string", () => {
  assert.deepEqual(parseShellContent({ stdout: "ok", stderr: "warn", exitCode: 2, aborted: true }), { stdout: "ok", stderr: "warn", exitCode: 2, aborted: true });
  assert.deepEqual(parseShellContent('{"stdout":"hi","exit_code":1}'), { stdout: "hi", stderr: "", exitCode: 1, aborted: false });
  assert.deepEqual(parseShellContent("plain text"), { stdout: "plain text", stderr: "", exitCode: 0, aborted: false });
});

test("authorizeRequest: single-tenant requires bearer==apiKey; multi-tenant gates on X-Bridge-Auth", () => {
  // Single-tenant (no bridge token): the Authorization bearer IS the gate and the key. UNCHANGED behavior.
  const st = (h) => authorizeRequestWith(h, { apiKey: "CKEY", bridgeToken: "" });
  assert.equal(st({ authorization: "Bearer CKEY" }), "CKEY"); // correct key -> use it
  assert.equal(st({ authorization: "Bearer WRONG" }), ""); // wrong key -> 401
  assert.equal(st({}), ""); // no auth -> 401
  // Multi-tenant (bridge token set): X-Bridge-Auth gates; the bearer is the PER-USER Cursor key.
  const mt = (h) => authorizeRequestWith(h, { apiKey: "DEFAULT", bridgeToken: "TOKEN" });
  assert.equal(mt({ "x-bridge-auth": "TOKEN", authorization: "Bearer userOneKey" }), "userOneKey");
  assert.equal(mt({ "x-bridge-auth": "TOKEN", authorization: "Bearer userTwoKey" }), "userTwoKey"); // distinct users -> distinct keys
  assert.equal(mt({ "x-bridge-auth": "WRONG", authorization: "Bearer userOneKey" }), ""); // bad gate -> 401
  assert.equal(mt({ authorization: "Bearer userOneKey" }), ""); // missing gate header -> 401
  assert.equal(mt({ "x-bridge-auth": "TOKEN" }), "DEFAULT"); // gate ok, no forwarded key -> bridge default
});

test("platformHasSession pins a key with ANY session incl. a paused one (activeRes=null) — HIGH#1 fix", () => {
  const h = keyHash("KEY_A");
  // A session paused between turns (awaiting tool_results: activeRes=null but its run is still live) MUST
  // still pin its platform, or enforcePlatformCap would dispose the sqlite stores out from under it.
  const paused = new Map([["s1", { cursorKey: "KEY_A", activeRes: null }]]);
  assert.equal(platformHasSession(h, paused), true);
  assert.equal(platformHasSession(keyHash("KEY_B"), paused), false); // no session on KEY_B -> evictable
  // An actively-streaming session pins too.
  const active = new Map([["s2", { cursorKey: "KEY_A", activeRes: {} }]]);
  assert.equal(platformHasSession(h, active), true);
});

test("ccToolId sanitizes to the Claude charset and uses full-uuid fallback (M2/L2)", () => {
  // Safe ids pass through unchanged (so they round-trip through Claude id-sanitization).
  assert.equal(ccToolId({ toolCallId: "toolu_abc-123_DEF" }), "toolu_abc-123_DEF");
  // Chars outside [a-zA-Z0-9_-] are replaced with _ (matching SanitizeClaudeToolID), so the echoed
  // tool_call_id still matches our pending key.
  assert.equal(ccToolId({ toolCallId: "call:abc/123.x=y z" }), "call_abc_123_x_y_z");
  // No id supplied => a full random uuid (not a truncated slice), prefixed and already in the safe charset.
  const a = ccToolId({}), b = ccToolId(undefined);
  assert.match(a, /^tc_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/);
  assert.notEqual(a, b); // unique per call
});

test("Session pending bookkeeping: resolve a subset leaves the rest pending (basis of the partial-batch fix)", () => {
  const s = new Session("t");
  const got = {};
  const mk = (id) => { const w = (c) => { got[id] = c; }; w.__reject = () => { got[id] = "__rejected__"; }; return w; };
  s.newPending("a", mk("a"));
  s.newPending("b", mk("b"));
  assert.equal(s.pending.size, 2);
  // Resolve only one of the batch -> the other stays pending (the partial condition the fix detects).
  assert.equal(s.resolvePending("a", "RESULT_A"), true);
  assert.equal(got.a, "RESULT_A");
  assert.equal(s.pending.size, 1);
  assert.equal(s.resolvePending("missing", "x"), false); // unknown id
  // The fix rejects the outstanding pendings so the run terminates instead of hanging.
  s.rejectAllPending("incomplete tool_results batch");
  assert.equal(s.pending.size, 0);
  assert.equal(got.b, "__rejected__");
});

test("Session.whenLogicalDone admits on REAL completion, not on a tool-pause settle (FIFO queue admission)", async () => {
  const s = new Session("q1");
  // No live run -> the next queued turn is admitted immediately.
  let immediate = false;
  await s.whenLogicalDone().then(() => { immediate = true; });
  assert.equal(immediate, true, "no run -> whenLogicalDone resolves immediately");

  // A live run paused for client tools: settle() fires (the per-HTTP-turn signal) but the run stays alive.
  // Admitting the next turn here would collide with the still-live run — the exact bug the queue must avoid.
  s.run = { id: "r" };
  let admitted = false;
  const waited = s.whenLogicalDone().then(() => { admitted = true; });
  s.settle();
  await Promise.resolve(); await Promise.resolve();
  assert.equal(admitted, false, "settle() at a tool-pause (run still live) must NOT admit the next turn");

  // Only real completion (run nulled + notifyLogicalDone) admits the next turn.
  s.run = null;
  s.notifyLogicalDone();
  await waited;
  assert.equal(admitted, true, "notifyLogicalDone() (real run completion) admits the next turn");
});

test("Session.hasQueuedWaiters drives depth-cap + eviction safety", () => {
  const s = new Session("q2");
  assert.equal(s.hasQueuedWaiters(), false);
  s.waiters = 3;
  assert.equal(s.hasQueuedWaiters(), true);
});

test("cancel() invalidates the in-flight run (done + runEpoch bump) so a late wait() cannot tear down a successor turn", async () => {
  const s = new Session("c1");
  s.run = { cancel: async () => {} };
  const ep0 = s.runEpoch;
  await s.cancel();
  assert.equal(s.done, true, "cancel must set done so a late onRunComplete short-circuits");
  assert.ok(s.runEpoch > ep0, "cancel must bump runEpoch to invalidate the cancelled run's wait() callback");
  // Simulate a successor turn having attached its response, then the OLD run's late wait() settling:
  let wroteToSuccessor = false;
  s.activeRes = { write() { wroteToSuccessor = true; } };
  s.onRunComplete({ status: "finished" }); // done===true -> must be a no-op
  assert.equal(wroteToSuccessor, false, "a late onRunComplete after cancel must NOT write to the successor turn's stream");
});

test("headlessRequestContext projects clientEnv and falls back to /workspace", () => {
  const def = headlessRequestContext(null).__ccJson.success.requestContext.env;
  assert.deepEqual(def.workspacePaths, ["/workspace"]);
  assert.equal(def.shell, "bash");
  const ce = headlessRequestContext({ workspacePaths: ["/home/u/p"], shell: "fish", processWorkingDirectory: "/home/u/p/x" }).__ccJson.success.requestContext.env;
  assert.deepEqual(ce.workspacePaths, ["/home/u/p"]);
  assert.equal(ce.shell, "fish");
  assert.equal(ce.processWorkingDirectory, "/home/u/p/x");
  assert.equal(ce.sandboxEnabled, false); // never sandboxed; tools route to the client
});
