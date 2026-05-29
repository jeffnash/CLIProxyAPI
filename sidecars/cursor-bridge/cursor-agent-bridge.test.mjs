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
  handleTurn,
  sessions,
  platforms,
  collectToolResultImages,
  isConversationTooLong,
  ensureAgent,
  CC_CASES,
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

test("emitToolUse buffers a late tool when no response is open; flushUndelivered delivers it next turn (Comment 1)", () => {
  const s = new Session("b1");
  const res = { write() {}, writeHead() {}, end() {}, on() {}, off() {} };
  s.activeRes = res;
  s.emitToolUse("A", "read", {});
  s.emitToolUse("B", "read", {});
  if (s.flushTimer) clearTimeout(s.flushTimer); // avoid the real 60ms timer firing after the test
  s.pauseForTools(); // close the turn -> turn_end{A,B}; delivered={A,B}
  assert.ok(s.delivered.has("A") && s.delivered.has("B"), "delivered tools are tracked");
  // Turn closed (the finally nulls activeRes). A late tool C must be BUFFERED, never silently lost as an
  // undeliverable pending.
  s.activeRes = null;
  s.emitToolUse("C", "read", {});
  assert.equal(s.undelivered.length, 1, "late tool with no open response must be buffered");
  assert.equal(s.undelivered[0].id, "C");
  assert.ok(!s.delivered.has("C"), "buffered tool is not yet delivered to the client");
  // Next turn opens a response; flushUndelivered delivers C so the client can answer it.
  s.activeRes = res;
  const flushed = s.flushUndelivered();
  assert.equal(flushed, true);
  assert.ok(s.delivered.has("C") && s.undelivered.length === 0, "buffered tool delivered on the next turn");
});

test("resolvePending is incremental + idempotent: a subset resolves, a re-sent id is a benign no-op not an error (Comment 2)", () => {
  const s = new Session("b2");
  const got = {};
  const mk = (id) => { const w = (c) => { got[id] = c; }; w.__reject = () => { got[id] = "__rej__"; }; return w; };
  s.newPending("A", mk("A"));
  s.newPending("B", mk("B"));
  assert.equal(s.resolvePending("A", "ra"), true);    // resolve only A
  assert.equal(s.pending.size, 1, "B stays pending — incremental answer must NOT error");
  assert.equal(s.resolvePending("A", "again"), false); // re-sent already-resolved id -> benign no-op (the retry case)
  assert.equal(s.resolvePending("B", "rb"), true);
  assert.equal(s.pending.size, 0);
  assert.equal(got.A, "ra");
  assert.equal(got.B, "rb");
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

test("tool_results for an unknown sessionId must NOT complete as a clean successful turn (Comment 1)", async () => {
  const id = "comment1-unknown-session-regression";
  sessions.delete(id); // guarantee the lookup misses regardless of test ordering
  let status = 0, sse = "", body = "", ended = false;
  const res = {
    writeHead(code) { status = code; return this; },
    write(s) { sse += s; return true; },
    end(s) { if (s != null) body += s; ended = true; },
    on() {}, off() {},
  };
  const req = { on() {}, off() {} };
  await handleTurn(req, res, {
    sessionId: id,
    input: { type: "tool_results", results: [{ toolCallId: "call_x", content: "RESULT" }] },
  }, "k");
  assert.ok(ended, "response must be terminated");
  assert.ok(!sessions.has(id), "the unknown session must NOT be silently created");
  // A clean successful terminal == a 2xx response carrying a success turn_end and no error. The bridge never
  // applied the tool result to a pending run (the session is gone), so it must NOT report success — that would
  // silently discard the client's tool work.
  const cleanSuccess = status >= 200 && status < 300 && /"stop_reason":"end_turn"/.test(sse) && !/error/i.test(sse);
  assert.ok(!cleanSuccess, `unknown-session tool_results must not complete as a clean success (status=${status} sse=${JSON.stringify(sse)} body=${JSON.stringify(body)})`);
  // It must be surfaced as a hard error: a non-2xx HTTP status (the Go executor rejects any non-2xx /agent/turn
  // response at its status check, before parsing or synthesizing a terminal for the stream).
  assert.ok(status >= 400, `expected an HTTP error status for a lost continuation, got status=${status} sse=${JSON.stringify(sse)} body=${JSON.stringify(body)}`);
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

// ───────────────────────── audit-fix regression tests (composer client-tools) ─────────────────────────
//
// These exercise the bridge-side audit fixes. The agent-driven paths (C1 fresh-send, C2 re-seed, C3 system
// swap, BR-DS resume) need a live SDK agent; we inject a FAKE platform into the exported `platforms` map so
// ensureAgent() resolves it without importing @cursor/sdk (which would pull native deps). The fake agent's
// send() records the message; its run.wait() resolves to a configurable RunResult.

// makeRes builds a mock SSE response that records status, the concatenated SSE payload, and close handlers.
function makeRes() {
  const closeHandlers = new Set();
  const r = {
    status: 0, sse: "", ended: false, headers: null,
    writeHead(code, h) { this.status = code; this.headers = h || null; return this; },
    write(s) { this.sse += s; return true; },
    end(s) { if (s != null) this.sse += s; this.ended = true; },
    on(ev, fn) { if (ev === "close") closeHandlers.add(fn); },
    off(ev, fn) { if (ev === "close") closeHandlers.delete(fn); },
    emitClose() { for (const fn of [...closeHandlers]) fn(); },
    closeHandlerCount() { return closeHandlers.size; },
  };
  return r;
}
const makeReq = () => ({ on() {}, off() {} });

// installFakePlatform injects a platform for cursorKey so ensureAgent resolves it (no real SDK). agentImpl
// is an object that may define send/getAgentMessages overrides; resumeAgent/createAgent return the agent.
function installFakePlatform(cursorKey, agentImpl, { resumeThrows = null, priorMessages = null } = {}) {
  const sends = [];
  const agent = {
    sends,
    send(msg, cbs) {
      sends.push({ msg, cbs });
      if (agentImpl && agentImpl.onSend) return agentImpl.onSend(msg, cbs);
      // default: a run that finishes cleanly with no streamed text.
      const run = { id: "run_" + sends.length, status: "finished", wait: () => Promise.resolve({ status: "finished" }), cancel: () => Promise.resolve() };
      return Promise.resolve(run);
    },
    close() {},
    reload() {},
  };
  const platform = {
    resumeAgent: async () => { if (resumeThrows) throw new Error(resumeThrows); return agent; },
    createAgent: async () => agent,
    getAgentMessages: async () => (Array.isArray(priorMessages) ? priorMessages : []),
  };
  platforms.set(keyHash(cursorKey), { promise: Promise.resolve(platform), stateRoot: "/tmp/fake", lastUsed: Date.now() });
  return { agent, platform, sends };
}

// drainTurn runs handleTurn for a continuation and waits for the response to terminate (the fake run
// resolves on a microtask, so a few awaits flush the settle->finally->[DONE] chain).
async function drainTurn(req, res, body, cursorKey) {
  const p = handleTurn(req, res, body, cursorKey);
  await p;
  for (let i = 0; i < 12 && !res.ended; i++) await Promise.resolve();
  return res;
}

// seedSession registers a pre-seeded, established Session in the sessions map with a fake platform behind it.
function seedSession(id, cursorKey, { advertise = [], seeded = true, seededSystem = "", historyFingerprint = null } = {}) {
  sessions.delete(id);
  const s = new Session(id, cursorKey);
  s.advertise = advertise;
  s.seeded = seeded;
  s.seededSystem = seededSystem;
  s.historyFingerprint = historyFingerprint;
  sessions.set(id, s);
  return s;
}

test("C4: toSdkImages accepts {url} and {url,mimeType}, still accepts {data,mimeType}, throws on neither", () => {
  // URL-only -> {url}; URL+mime -> {url,mimeType}.
  assert.deepEqual(toSdkImages([{ url: "https://x/y.png" }]), [{ url: "https://x/y.png" }]);
  assert.deepEqual(toSdkImages([{ url: "https://x/y.png", mimeType: "image/png" }]), [{ url: "https://x/y.png", mimeType: "image/png" }]);
  // Inline base64 still works unchanged.
  assert.deepEqual(toSdkImages([{ data: "QUJD", mimeType: "image/png" }]), [{ data: "QUJD", mimeType: "image/png" }]);
  // Mixed batch.
  assert.deepEqual(
    toSdkImages([{ data: "QUJD", mimeType: "image/jpeg" }, { url: "http://h/z.gif" }]),
    [{ data: "QUJD", mimeType: "image/jpeg" }, { url: "http://h/z.gif" }],
  );
  // Neither a valid url nor data+mimeType -> throws (parity with inline validation).
  assert.throws(() => toSdkImages([{ foo: "bar" }]), /url/);
  assert.throws(() => toSdkImages([{ url: "" }]), /data\/mimeType or url/);
});

test("collectToolResultImages gathers tr.images across results (BR9/EX3)", () => {
  const imgs = collectToolResultImages({ results: [
    { toolCallId: "a", content: "x", images: [{ data: "Q", mimeType: "image/png" }] },
    { toolCallId: "b", content: "y" },
    { toolCallId: "c", content: "z", images: [{ url: "https://h/i.png" }] },
  ] });
  assert.deepEqual(imgs, [{ data: "Q", mimeType: "image/png" }, { url: "https://h/i.png" }]);
  assert.deepEqual(collectToolResultImages({ results: [{ toolCallId: "a", content: "x" }] }), []);
  assert.deepEqual(collectToolResultImages({}), []);
});

test("isConversationTooLong matches the Cursor error class (BR-PL)", () => {
  assert.equal(isConversationTooLong("ERROR_CONVERSATION_TOO_LONG"), true);
  assert.equal(isConversationTooLong("upstream: error_conversation_too_long (run failed)"), true);
  assert.equal(isConversationTooLong("the conversation is too long"), true);
  assert.equal(isConversationTooLong("some other error"), false);
  assert.equal(isConversationTooLong(""), false);
  assert.equal(isConversationTooLong(null), false);
});

test("BR4/C5: resolvePending threads isError into the dispatchMcp result", async () => {
  const s = new Session("br4");
  // Wire a real dispatchMcp pending so the wrap builds the McpResult shape.
  s.advertise = [{ name: "T", toolName: "T" }];
  const p = s.dispatchMcp({ toolName: "T", args: {} });
  // Resolve as a FAILED tool.
  const idEmitted = s.everEmitted.size ? [...s.everEmitted][0] : null;
  assert.ok(idEmitted, "dispatchMcp must record the emitted id in everEmitted (BR1)");
  assert.equal(s.resolvePending(idEmitted, "boom", true), true);
  const out = await p;
  assert.equal(out.__ccJson.success.isError, true, "isError must propagate to the McpResult");
  assert.equal(out.__ccJson.success.content[0].text.text, "boom");
  // And the success case stays isError:false.
  const s2 = new Session("br4b");
  s2.advertise = [{ name: "T", toolName: "T" }];
  const p2 = s2.dispatchMcp({ toolName: "T", args: {} });
  s2.resolvePending([...s2.everEmitted][0], "ok"); // default isError=false
  const out2 = await p2;
  assert.equal(out2.__ccJson.success.isError, false);
});

test("BR8/C5: native shell failure surfaces as a non-zero exit even when content exitCode is 0", () => {
  // buildResult honors isError: exitCode 0 -> forced 1.
  const r = CC_CASES.shellArgs.buildResult("plain stdout", { command: "ls" }, true);
  assert.equal(r.success.exitCode, 1, "isError shell with exitCode 0 must report a non-zero exit");
  // A real non-zero exit is preserved.
  const r2 = CC_CASES.shellArgs.buildResult(JSON.stringify({ stdout: "x", exitCode: 7 }), { command: "ls" }, true);
  assert.equal(r2.success.exitCode, 7);
  // Without isError, exit stays 0.
  const r3 = CC_CASES.shellArgs.buildResult("ok", { command: "ls" }, false);
  assert.equal(r3.success.exitCode, 0);
  // Streaming: the exit chunk gets a non-zero code + aborted when isError.
  const chunks = CC_CASES.shellStreamArgs.buildChunks("out", true);
  const exit = chunks.find((c) => c.exit);
  assert.equal(exit.exit.code, 1);
  assert.equal(exit.exit.aborted, true);
});

// makeDeferred returns a resolvable promise (used to model a live SDK run whose wait() completes when the
// pending tool is answered, mirroring how a real continuation resumes a paused run).
function makeDeferred() {
  let resolve;
  const promise = new Promise((r) => { resolve = r; });
  return { promise, resolve };
}

test("BR7: tool_results id-miss with exactly one pending resolves the lone pending", async () => {
  const id = "br7-session";
  const cursorKey = "k-br7";
  const s = seedSession(id, cursorKey, { seeded: true });
  const { sends } = installFakePlatform(cursorKey, null);
  // Model a LIVE paused run awaiting one tool. Answering the lone pending resolves the run -> onRunComplete
  // settles the turn (exactly how a real continuation resumes). A fresh send must NOT be spawned.
  const runDone = makeDeferred();
  s.run = { id: "live", wait: () => runDone.promise, cancel: async () => {} };
  const got = {};
  const wrap = (c, e) => { got.content = c; got.isError = e; runDone.resolve({ status: "finished" }); };
  wrap.__reject = () => {};
  s.newPending("real-id", wrap);
  s.everEmitted.add("real-id"); s.delivered.add("real-id");
  s.run.wait().then((r) => s.onRunComplete(r)).catch((e) => s.onRunError(e)); // mirror driveUserSend's wiring
  // Continuation answers with a DIFFERENT id (Gemini id mismatch) but only ONE tool is pending.
  const res = makeRes();
  await drainTurn(makeReq(), res, { sessionId: id, input: { type: "tool_results", results: [{ toolCallId: "gemini-mismatched-id", content: "ANSWER" }] } }, cursorKey);
  assert.equal(got.content, "ANSWER", "the lone pending must be resolved with the mismatched-id result");
  assert.equal(sends.length, 0, "resolving the lone pending lets the run resume; a fresh send is NOT spawned");
});

test("BR1: tool_results for a never-emitted id surfaces an error turn (not a clean success)", async () => {
  const id = "br1-unknown";
  const cursorKey = "k-br1";
  seedSession(id, cursorKey, { seeded: true });
  installFakePlatform(cursorKey, null);
  const res = makeRes();
  await drainTurn(makeReq(), res, { sessionId: id, input: { type: "tool_results", results: [{ toolCallId: "never-issued", content: "x" }] } }, cursorKey);
  assert.match(res.sse, /"stop_reason":"error"/, "an id never issued by this session must be an error turn");
  assert.match(res.sse, /unknown tool_call_id never-issued/);
});

test("BR1: a watchdog-reaped / already-emitted id is benign (no error, no false success masking)", async () => {
  const id = "br1-reaped";
  const cursorKey = "k-br1b";
  const s = seedSession(id, cursorKey, { seeded: true });
  installFakePlatform(cursorKey, null);
  // Simulate a tool that WAS emitted (so it is in everEmitted) but whose pending was reaped by the watchdog
  // (no longer in pending, and not in `delivered` after clearTurnState).
  s.everEmitted.add("reaped-id");
  const res = makeRes();
  await drainTurn(makeReq(), res, { sessionId: id, input: { type: "tool_results", results: [{ toolCallId: "reaped-id", content: "late" }] } }, cursorKey);
  assert.doesNotMatch(res.sse, /"stop_reason":"error"/, "an ever-emitted (reaped) id must NOT error");
  assert.match(res.sse, /"stop_reason":"end_turn"/, "it is acked benignly");
});

test("BR2: matched===0 after a paused run died with lastRunError surfaces the error (not end_turn)", async () => {
  const id = "br2";
  const cursorKey = "k-br2";
  const s = seedSession(id, cursorKey, { seeded: true });
  installFakePlatform(cursorKey, null);
  // The paused run died upstream: run nulled, lastRunError set, the answered id is no longer pending but was
  // ever emitted (so it is benign, not "unknown") — the ONLY signal left is lastRunError.
  s.run = null;
  s.lastRunError = "ERROR_PARALLEL_TOOL_UPSTREAM";
  s.everEmitted.add("dead-tool");
  const res = makeRes();
  await drainTurn(makeReq(), res, { sessionId: id, input: { type: "tool_results", results: [{ toolCallId: "dead-tool", content: "x" }] } }, cursorKey);
  assert.match(res.sse, /"stop_reason":"error"/, "a died-run continuation must surface the error, not a clean turn");
  assert.match(res.sse, /ERROR_PARALLEL_TOOL_UPSTREAM/);
  assert.equal(s.lastRunError, null, "lastRunError is cleared after being surfaced");
});

test("BR5/C1: tool_results with userText and nothing to resume drives a fresh agent.send", async () => {
  const id = "br5";
  const cursorKey = "k-br5";
  seedSession(id, cursorKey, { seeded: true, advertise: [{ name: "Read", toolName: "Read" }] });
  const { sends } = installFakePlatform(cursorKey, null);
  const res = makeRes();
  await drainTurn(makeReq(), res, {
    sessionId: id,
    input: { type: "tool_results", results: [{ toolCallId: "stale", content: "x" }], userText: "now do the next thing" },
  }, cursorKey);
  assert.equal(sends.length, 1, "a fresh user send must be driven for the mixed-turn user message");
  const sent = sends[0].msg;
  const text = typeof sent === "string" ? sent : sent.text;
  assert.match(text, /now do the next thing/, "the user's trailing message is sent to the model");
  assert.doesNotMatch(res.sse, /"stop_reason":"error"/);
});

test("BR5/C1: a continuation that answers ALL pending with a live run RESUMES (no fresh send, no cancel)", async () => {
  // Regression guard: a mixed turn where the client answered every pending tool AND included userText must
  // let the paused run RESUME (the userText rode along folded into the tool result). A naive C1 (fire on
  // pending===0) would cancel the resuming run and re-send only the userText, dropping the model's answer.
  const id = "br5-resume";
  const cursorKey = "k-br5-resume";
  const s = seedSession(id, cursorKey, { seeded: true });
  const { sends } = installFakePlatform(cursorKey, null);
  let canceled = false;
  s.cancel = async () => { canceled = true; };
  const runDone = makeDeferred();
  s.run = { id: "live", wait: () => runDone.promise, cancel: async () => {} };
  const got = {};
  const wrap = (c) => { got.content = c; runDone.resolve({ status: "finished" }); }; wrap.__reject = () => {};
  s.newPending("tool-1", wrap); s.everEmitted.add("tool-1"); s.delivered.add("tool-1");
  s.run.wait().then((r) => s.onRunComplete(r)).catch((e) => s.onRunError(e));
  const res = makeRes();
  await drainTurn(makeReq(), res, {
    sessionId: id,
    input: { type: "tool_results", results: [{ toolCallId: "tool-1", content: "RESULT" }], userText: "and also note this" },
  }, cursorKey);
  assert.equal(got.content, "RESULT", "the pending tool is resolved and the run resumes");
  assert.equal(canceled, false, "the resuming run must NOT be cancelled by C1");
  assert.equal(sends.length, 0, "no separate fresh send when the run resumes (userText rode the tool result)");
});

test("BR5/C1: tool-result images are folded into the C1 fresh send", async () => {
  const id = "br5img";
  const cursorKey = "k-br5img";
  seedSession(id, cursorKey, { seeded: true });
  const { sends } = installFakePlatform(cursorKey, null);
  const res = makeRes();
  await drainTurn(makeReq(), res, {
    sessionId: id,
    input: { type: "tool_results", results: [{ toolCallId: "s", content: "x", images: [{ url: "https://h/i.png" }] }], userText: "see this", images: [{ data: "QQ", mimeType: "image/png" }] },
  }, cursorKey);
  assert.equal(sends.length, 1);
  const sent = sends[0].msg;
  assert.ok(sent && Array.isArray(sent.images), "the C1 send carries images");
  // Both the input.images and the tool-result image survive (order: input images first, then tool-result).
  assert.deepEqual(sent.images, [{ data: "QQ", mimeType: "image/png" }, { url: "https://h/i.png" }]);
});

test("BR6/C3: a changed system on a continuation is applied to the C1 send + seededSystem updated", async () => {
  const id = "br6";
  const cursorKey = "k-br6";
  const s = seedSession(id, cursorKey, { seeded: true, seededSystem: "OLD SYSTEM" });
  const { sends } = installFakePlatform(cursorKey, null);
  const res = makeRes();
  await drainTurn(makeReq(), res, {
    sessionId: id,
    input: { type: "tool_results", results: [{ toolCallId: "s", content: "x" }], userText: "continue", system: "NEW PLAN-MODE SYSTEM" },
  }, cursorKey);
  assert.equal(sends.length, 1);
  const text = typeof sends[0].msg === "string" ? sends[0].msg : sends[0].msg.text;
  assert.match(text, /NEW PLAN-MODE SYSTEM/, "the swapped system must reach the model");
  assert.match(text, /continue/);
  assert.equal(s.seededSystem, "NEW PLAN-MODE SYSTEM", "seededSystem is updated to the new system");
});

test("BR-C2: a changed historyFingerprint on an established session (no live run) cancels + re-seeds", async () => {
  const id = "brc2";
  const cursorKey = "k-brc2";
  const s = seedSession(id, cursorKey, { seeded: true, historyFingerprint: "old-fp-0000000000000000000000000" });
  let cancelCalls = 0;
  const origCancel = s.cancel.bind(s);
  s.cancel = async () => { cancelCalls++; return origCancel(); };
  const { sends } = installFakePlatform(cursorKey, null);
  const res = makeRes();
  await drainTurn(makeReq(), res, {
    sessionId: id,
    input: { type: "tool_results", results: [{ toolCallId: "stale", content: "x" }], userText: "after compact", history: "U: earlier\nA: summary", system: "SYS", historyFingerprint: "new-fp-1111111111111111111111111" },
  }, cursorKey);
  assert.ok(cancelCalls >= 1, "a changed fingerprint must cancel the stale run");
  assert.equal(sends.length, 1, "re-seed drives a fresh send");
  const text = typeof sends[0].msg === "string" ? sends[0].msg : sends[0].msg.text;
  assert.match(text, /Previous conversation:\n[\s\S]*summary/, "re-seed prepends the replaced history");
  assert.match(text, /after compact/, "the trailing user text is sent");
  assert.equal(s.historyFingerprint, "new-fp-1111111111111111111111111", "the new fingerprint is stored");
});

test("BR-C2: a changed historyFingerprint while a run is LIVE does NOT cancel (protects the continuation)", async () => {
  const id = "brc2-live";
  const cursorKey = "k-brc2-live";
  const s = seedSession(id, cursorKey, { seeded: true, historyFingerprint: "old-fp" });
  installFakePlatform(cursorKey, null);
  // A live paused run with a pending tool: the continuation is answering it. A growth fingerprint change must
  // NOT tear this down (that would silently lose the in-flight tool work).
  let canceled = false;
  s.cancel = async () => { canceled = true; };
  const runDone = makeDeferred();
  s.run = { id: "live", wait: () => runDone.promise, cancel: async () => {} };
  const got = {};
  const wrap = (c) => { got.content = c; runDone.resolve({ status: "finished" }); }; wrap.__reject = () => {};
  s.newPending("live-tool", wrap); s.everEmitted.add("live-tool"); s.delivered.add("live-tool");
  s.run.wait().then((r) => s.onRunComplete(r)).catch((e) => s.onRunError(e));
  const res = makeRes();
  await drainTurn(makeReq(), res, {
    sessionId: id,
    input: { type: "tool_results", results: [{ toolCallId: "live-tool", content: "RESULT" }], historyFingerprint: "grown-fp" },
  }, cursorKey);
  assert.equal(canceled, false, "a live run must NOT be cancelled by a fingerprint change");
  assert.equal(got.content, "RESULT", "the pending tool is resolved (the run resumes)");
  assert.equal(s.historyFingerprint, "grown-fp", "the fingerprint is still updated for the next comparison");
});

test("BR-DS: ensureAgent resume that finds prior turns marks the session seeded (no double-seed)", async () => {
  const id = "brds";
  const cursorKey = "k-brds";
  const s = new Session(id, cursorKey);
  s.seeded = false;
  installFakePlatform(cursorKey, null, { priorMessages: [{ type: "user", uuid: "1", agent_id: id, message: {} }] });
  await ensureAgent(s, "composer-2.5");
  assert.equal(s.seeded, true, "a resume with prior turns must set seeded=true so history is not re-prepended");
});

test("BR-DS: ensureAgent resume with NO prior turns leaves seeded as-is", async () => {
  const id = "brds-empty";
  const cursorKey = "k-brds-empty";
  const s = new Session(id, cursorKey);
  s.seeded = false;
  installFakePlatform(cursorKey, null, { priorMessages: [] });
  await ensureAgent(s, "composer-2.5");
  assert.equal(s.seeded, false, "no prior turns -> seeded stays false (the next send seeds)");
});

test("BR-PL: onRunError on a conversation-too-long error deletes the session + cancels", () => {
  const id = "brpl";
  const s = seedSession(id, "k-brpl", { seeded: true });
  assert.ok(sessions.has(id));
  s.onRunError(new Error("upstream ERROR_CONVERSATION_TOO_LONG"));
  assert.ok(!sessions.has(id), "a poisoned long session must be dropped so the next turn re-seeds fresh");
});

test("BR3: a disconnected QUEUED waiter self-reaps synchronously (frees the slot without waiting on the run ahead)", async () => {
  const id = "br3";
  const cursorKey = "k-br3";
  const s = seedSession(id, cursorKey, { seeded: true });
  installFakePlatform(cursorKey, null);
  // Park the FIFO: a never-resolving prior tail so the queued waiter cannot be promoted yet.
  let releasePrev;
  s.tail = new Promise((r) => { releasePrev = r; });
  // Enqueue a NEW-USER turn: it becomes a waiter behind the parked tail.
  const res = makeRes();
  const p = handleTurn(makeReq(), res, { sessionId: id, input: { type: "user", text: "queued" } }, cursorKey);
  await Promise.resolve();
  assert.equal(s.waiters, 1, "the new-user turn is queued as a waiter");
  // The client disconnects while still queued -> the waiter must self-reap NOW, not after the run ahead.
  res.emitClose();
  assert.equal(s.waiters, 0, "waiters is decremented synchronously on disconnect (BR3)");
  assert.ok(res.ended, "the abandoned waiter's response is ended immediately");
  // Releasing the parked tail must NOT double-decrement waiters (guarded by `reaped`).
  releasePrev();
  await p.catch(() => {});
  for (let i = 0; i < 8; i++) await Promise.resolve();
  assert.equal(s.waiters, 0, "no double-decrement after the tail releases");
  sessions.delete(id);
});
