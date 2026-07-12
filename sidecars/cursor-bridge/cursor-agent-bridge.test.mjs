import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { createHash } from "node:crypto";
import { EventEmitter } from "node:events";
import { existsSync, mkdirSync, mkdtempSync, readdirSync, readFileSync, rmSync, utimesSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { Readable } from "node:stream";
import test from "node:test";

const TEST_STATE_ROOT = mkdtempSync(path.join(tmpdir(), "cursor-bridge-state-"));
process.env.CURSOR_AGENT_STATE_ROOT = TEST_STATE_ROOT;
process.env.CURSOR_AGENT_TOOL_BATCH_MS = "60000";
process.env.CURSOR_AGENT_PENDING_TIMEOUT_MS = "60000";

const bridge = await import("./cursor-agent-bridge.mjs");
const { createRoundInfrastructure, ToolRound, ToolRoundError, TerminalReason } = await import("./tool-round.mjs");
const {
  AdvertisedToolRegistry,
  appendRulesReminder,
  CC_CASES,
  CLIENT_TOOL_PROVIDER_ID,
  COMPOSER_LIVE_TOOL_RESULT_MAX_BYTES,
  COMPOSER_MAX_IDENTICAL_INVALID_TOOL_CALLS,
  COMPOSER_MAX_INVALID_TOOL_CALLS,
  DEFAULT_MCP_SERVER_KEY,
  Session,
  authorizeRequestWith,
  bindHostIsLoopback,
  blockedNativeResult,
  blockedSyntheticNativeExecIfNeeded,
  buildMcpServers,
  buildReadSuccess,
  buildRestartRecoveryInput,
  buildWriteSuccess,
  collectToolResultImages,
  composerModelSelection,
  composerWorkspaceCwd,
  completedTurnReceipts,
  completedTurnRequestHash,
  cleanupCompletedTurnReceipts,
  readFreshDeliveryReceipt,
  runBoundedShutdownTasks,
  runDurableMaintenance,
  turnInvocationIdentity,
  validCompletedTurnReceipt,
  continuationTenantMismatch,
  constraintInstructions,
  dispatchMcpBatch,
  effectiveAdvertise,
  envInt,
  handleContinue,
  handleHttpRequestSafely,
  handleTurn,
  headlessMcpState,
  headlessRequestContext,
  healthBody,
  isConversationTooLong,
  isLoopbackRemote,
  classifyMcpRoute,
  keyHash,
  keyFingerprint,
  liveToolRounds,
  MAX_AGENT_TURN_BYTES,
  mcpDispatch,
  mcpDispatchResult,
  mcpImageResultsEnabled,
  mcpServerKeyForTool,
  mcpToolsForServer,
  sseFrameSizeError,
  nativeToolBlockedByChoice,
  parseShellContent,
  platforms,
  prepareAdvertisedToolRegistry,
  readinessBody,
  replayMemoryBudget,
  readBodyBounded,
  reconcileExport,
  refreshSessionFromBody,
  sessions,
  sdkAdvertisedTools,
  sdkSendIdempotencyKey,
  stateRootDiskStatus,
  systemContextPlan,
  normalizeSystemBlocks,
  syntheticAgentArtifactFailure,
  syntheticAgentArtifactRequest,
  toolManifest,
  toolManifestRule,
  toolResultRecoveryPlan,
  terminalizePoisonedPlatformSessions,
  consumeExpectedSdkAbort,
  consumeExpectedSdkLifecycleClosure,
  isClosedInputStreamError,
  isSdkAbortError,
  isSdkIteratorClosedError,
  noteExpectedSdkAbort,
  toSdkImages,
  truncateLiveToolResult,
  typedUnavailableResult,
  validateBindHost,
  wrapToolInput,
} = bridge;

test("process-wide replay memory is reserved before a turn and rejects before SDK send", async () => {
  const originalLimit = replayMemoryBudget.limit;
  const originalUsed = replayMemoryBudget.used;
  const first = new Session(`replay-budget-a-${Date.now()}`, "cursor-key-a");
  const second = new Session(`replay-budget-b-${Date.now()}`, "cursor-key-b");
  const initialBytes = 2 * 1024 * 1024; // adaptive initial reservation
  try {
    replayMemoryBudget.limit = initialBytes;
    replayMemoryBudget.used = 0;
    first.beginReplaySegment("turn-a");
    assert.equal(replayMemoryBudget.used, initialBytes);
    assert.throws(
      () => second.beginReplaySegment("turn-b"),
      (error) => error.code === "local_replay_capacity" && error.status === 503,
    );
    const blockedSessionId = `replay-budget-http-${Date.now()}`;
    const blocked = new MockResponse();
    const writeHead = blocked.writeHead.bind(blocked);
    let headerWrites = 0;
    blocked.writeHead = (status, headers = {}) => {
      if (headerWrites++ > 0) throw new Error("ERR_HTTP_HEADERS_SENT");
      return writeHead(status, headers);
    };
    await handleTurn(
      request(),
      blocked,
      neutralBody({
        sessionId: blockedSessionId,
        input: { type: "user", text: "must not reach the SDK", clientMessageId: "ccm2_replay_budget_blocked" },
      }),
      "cursor-key-blocked",
    );
    assert.equal(blocked.status, 200);
    assert.equal(headerWrites, 1);
    assert.match(blocked.chunks.join(""), /"errorCode":"local_replay_capacity"/);
    assert.equal(sessions.has(blockedSessionId), false);
    first.beginReplaySegment("turn-a");
    assert.equal(replayMemoryBudget.used, initialBytes, "same-turn reseed must reuse its reservation");
    first.beginReplaySegment();
    assert.equal(replayMemoryBudget.used, 0);
    second.beginReplaySegment("turn-b");
    assert.equal(replayMemoryBudget.used, initialBytes);
  } finally {
    first.beginReplaySegment();
    second.beginReplaySegment();
    replayMemoryBudget.limit = originalLimit;
    replayMemoryBudget.used = originalUsed;
  }
});


class MockResponse extends EventEmitter {
  constructor(writeReturns = []) {
    super();
    this.writeReturns = [...writeReturns];
    this.status = 0;
    this.headers = {};
    this.headersSent = false;
    this.chunks = [];
    this.ended = false;
  }
  setHeader(name, value) { this.headers[String(name).toLowerCase()] = value; }
  getHeader(name) { return this.headers[String(name).toLowerCase()]; }
  writeHead(status, headers = {}) {
    this.status = status;
    this.headersSent = true;
    for (const [key, value] of Object.entries(headers)) this.setHeader(key, value);
    return this;
  }
  write(chunk) {
    this.chunks.push(String(chunk));
    return this.writeReturns.length ? this.writeReturns.shift() : true;
  }
  end(chunk = undefined) {
    if (chunk !== undefined) this.chunks.push(String(chunk));
    this.ended = true;
  }
  text() { return this.chunks.join(""); }
  json() { return JSON.parse(this.text()); }
}

function request(remoteAddress = "127.0.0.1") {
  const req = new EventEmitter();
  req.headers = {};
  req.socket = { remoteAddress };
  return req;
}

function neutralBody(body = {}) {
  return { ...body, clientEnv: { workspaceUnknown: true } };
}

function authoritativeRawToolsBody(toolInventoryJSON, extra = {}) {
  return {
    contractVersion: 2,
    toolsAuthoritative: true,
    toolInventoryJSON,
    toolInventoryEpoch: "cti1_" + createHash("sha256").update(toolInventoryJSON, "utf8").digest("hex").slice(0, 32),
    ...extra,
  };
}

function authoritativeToolsBody(tools, extra = {}) {
  return authoritativeRawToolsBody(JSON.stringify(tools), extra);
}

function advertised(name = "Lookup") {
  return [{ name, toolName: name, description: `${name} tool`, inputSchema: { type: "object", properties: { q: { type: "string" } } } }];
}

test("continuation terminals echo the exact durable fresh-turn lease", async (t) => {
  const decode = (res) => JSON.parse(
    res.text().split("\n").find((line) => line.startsWith("data: {")).slice("data: ".length),
  );

  const paused = new Session("sess_leaseecho_paused");
  paused.clientLeaseToken = "18446744073709551615";
  const round = paused.ensureToolRound();
  t.after(() => {
    try { round.terminalize(TerminalReason.CLIENT_CANCELLED, "test cleanup"); } catch {}
    sessions.delete(paused.id);
  });
  assert.equal(round.clientLeaseToken, "18446744073709551615");
  assert.equal(ToolRound.load(
    createRoundInfrastructure(TEST_STATE_ROOT).journal,
    createRoundInfrastructure(TEST_STATE_ROOT).codec,
    round.route,
  ).clientLeaseToken, "18446744073709551615");

  const pausedRes = new MockResponse();
  paused.beginResponse(pausedRes);
  assert.equal(paused.sse({ type: "turn_end", stop_reason: "tool_use" }), true);
  await paused.finishResponse();
  assert.deepEqual(decode(pausedRes).clientLease, {
    sessionId: paused.id,
    token: "18446744073709551615",
    terminal: false,
  });

  const finished = new Session("sess_leaseecho_finished");
  finished.clientLeaseToken = "42";
  t.after(() => sessions.delete(finished.id));
  const finishedRes = new MockResponse();
  finished.beginResponse(finishedRes);
  assert.equal(finished.sse({ type: "turn_end", stop_reason: "end_turn" }), true);
  await finished.finishResponse();
  assert.deepEqual(decode(finishedRes).clientLease, {
    sessionId: finished.id,
    token: "42",
    terminal: true,
  });

  const rejectedRes = new MockResponse();
  finished.responseWriter = null;
  finished.activeRes = null;
  finished.beginResponse(rejectedRes);
  assert.equal(finished.sse({
    type: "turn_end",
    stop_reason: "error",
    _clientLeaseTerminal: false,
  }), true);
  await finished.finishResponse();
  const rejected = decode(rejectedRes);
  assert.equal(rejected.clientLease.terminal, false);
  assert.equal(rejected._clientLeaseTerminal, undefined, "bridge-only lease controls must never leak to clients");
});

function seedSession(id, key = "cursor-key", tools = advertised()) {
  const session = new Session(id, key);
  session.clientEnv = { workspaceUnknown: true };
  session.advertise = tools;
  const output = new MockResponse();
  session.beginResponse(output);
  sessions.set(id, session);
  return { session, output };
}

async function waitFor(predicate, message = "condition") {
  for (let i = 0; i < 100; i++) {
    if (predicate()) return;
    await new Promise((resolve) => setImmediate(resolve));
  }
  assert.fail(`timed out waiting for ${message}`);
}

async function openTool(session, { name = "Lookup", rawId = "sdk-call", input = { q: "x" }, adapter = null, awaiting = true } = {}) {
  const promise = session.openClientTool({ source: "test", rawToolCallId: rawId, name, input, resultAdapter: adapter || ((value) => value) });
  promise.catch(() => {});
  await waitFor(() => session.currentRound && session.currentRound.fifo.length === 1, "journaled tool call");
  const round = session.currentRound;
  const call = round.calls.get(round.fifo[0]);
  await waitFor(() => call.handedAt != null, "transport handoff receipt");
  if (session.flushTimer) { clearTimeout(session.flushTimer); session.flushTimer = null; }
  if (awaiting) round.markAwaitingResults();
  return { call, promise, round };
}

function continuationBody(results, extraInput = {}, extraBody = {}) {
  return neutralBody({
    model: "cursor-grok-4.5",
    input: { type: "tool_results", results, ...extraInput },
    ...extraBody,
  });
}

function journalRecord(route) {
  return JSON.parse(readFileSync(path.join(TEST_STATE_ROOT, "client-tool-rounds", `${route}.json`), "utf8"));
}

function exactTurnReceiptFile(cursorKey, sessionId, clientMessageId, input) {
  const requestHash = completedTurnRequestHash(input);
  const scope = createHash("sha256")
    .update(keyFingerprint(cursorKey))
    .update("\0")
    .update(String(sessionId))
    .update("\0")
    .update(String(clientMessageId))
    .update("\0")
    .update(requestHash)
    .digest("hex");
  return path.join(TEST_STATE_ROOT, ".cct-completed-turns", `${scope}.json`);
}

function drainExpectedSdkTickets() {
  for (let i = 0; i < 16384; i++) {
    if (!consumeExpectedSdkAbort(new Map())) return;
  }
  throw new Error("expected SDK cancellation tickets did not remain bounded");
}

async function cleanupState() {
  for (const session of sessions.values()) {
    try { await session.cancel(); } catch {}
  }
  sessions.clear();
  liveToolRounds.clear();
  platforms.clear();
  completedTurnReceipts.clear();
  drainExpectedSdkTickets();
}

test.beforeEach(cleanupState);
test.after(async () => {
  await cleanupState();
  rmSync(TEST_STATE_ROOT, { recursive: true, force: true });
});

test("expected SDK cancellation AbortErrors are classified without consuming fatal paths", () => {
  assert.equal(isSdkAbortError({ name: "AbortError", message: "This operation was aborted" }), true);
  assert.equal(isSdkAbortError(new Error("ordinary SDK failure")), false);
  const sessionsMap = new Map([["cancelled", { done: true, run: null }], ["live", { done: false, run: {} }]]);
  noteExpectedSdkAbort("cancelled");
  assert.equal(consumeExpectedSdkAbort(sessionsMap), true);
  drainExpectedSdkTickets();
  noteExpectedSdkAbort("live");
  assert.equal(consumeExpectedSdkAbort(sessionsMap), false);
  sessionsMap.get("live").done = true;
  assert.equal(consumeExpectedSdkAbort(sessionsMap), true);
  drainExpectedSdkTickets();
});

test("Cursor SDK iterable closures are recognized as cancellation lifecycle errors", () => {
  const writeClosed = { name: "WriteIterableClosedError", message: "WritableIterable is closed" };
  const iteratorClosed = new Error("Iterator was closed");
  assert.equal(isClosedInputStreamError(writeClosed), true);
  assert.equal(isSdkIteratorClosedError(iteratorClosed), true);
  assert.equal(isClosedInputStreamError(new Error("ordinary failure")), false);
  assert.equal(isSdkIteratorClosedError(new Error("ordinary failure")), false);

  noteExpectedSdkAbort("recently-cancelled");
  assert.equal(consumeExpectedSdkLifecycleClosure(), true);
  assert.equal(consumeExpectedSdkLifecycleClosure(), true);
  drainExpectedSdkTickets();
  assert.equal(consumeExpectedSdkLifecycleClosure(), false);
});

test("SDK cancellation tickets expire and stay entry- and match-bounded", () => {
  const realNow = Date.now;
  let fakeNow = realNow();
  Date.now = () => fakeNow;
  try {
    noteExpectedSdkAbort("expired", 1);
    fakeNow += 10_001;
    assert.equal(consumeExpectedSdkAbort(new Map()), false, "expired tickets must not classify later failures");

    for (let i = 0; i < 2000; i++) noteExpectedSdkAbort(`cancel-${i}`, i);
    let matches = 0;
    while (consumeExpectedSdkAbort(new Map())) matches++;
    assert.equal(matches, 1024 * 8, "ticket entry and per-ticket match caps must remain deterministic");
  } finally {
    Date.now = realNow;
    drainExpectedSdkTickets();
  }
});

test("exact cancellation async context absorbs a multi-error leak after the same session starts a successor", async () => {
  const moduleUrl = new URL("./cursor-agent-bridge.mjs", import.meta.url).href;
  const code = `
    const bridge = await import(${JSON.stringify(moduleUrl)});
    const sessionId = "cancelled-then-replaced";
    const cancelled = new bridge.Session(sessionId, "key");
    cancelled.runEpoch = 7;
    cancelled.run = {
      async cancel() {
        const closed = Object.assign(new Error("WritableIterable is closed"), { name: "WriteIterableClosedError" });
        setTimeout(() => Promise.reject(closed), 0);
        setTimeout(() => Promise.reject(Object.assign(new Error("This operation was aborted"), { name: "AbortError" })), 15);
        setTimeout(() => Promise.reject(new Error("Iterator was closed")), 30);
      }
    };
    cancelled.agent = { async close() {} };
    bridge.sessions.set(sessionId, cancelled);
    await cancelled.cancel();
    const successor = { run: {}, activeRes: {}, done: false, runEpoch: 8, aborted: false, abortFromSdk() { this.aborted = true; } };
    bridge.sessions.set(sessionId, successor);
    setTimeout(() => {
      process.stdout.write("BRIDGE_ALIVE sessions=" + bridge.sessions.size + " successorAborted=" + successor.aborted + "\\n");
      process.exit(0);
    }, 100);
  `;
  const child = spawn(process.execPath, ["--input-type=module", "--eval", code], {
    cwd: path.dirname(new URL(import.meta.url).pathname),
    env: { ...process.env, CURSOR_AGENT_STATE_ROOT: TEST_STATE_ROOT },
    stdio: ["ignore", "pipe", "pipe"],
  });
  let stdout = "";
  let stderr = "";
  child.stdout.on("data", (chunk) => { stdout += chunk; });
  child.stderr.on("data", (chunk) => { stderr += chunk; });
  const exit = await new Promise((resolve, reject) => {
    child.once("error", reject);
    child.once("exit", (codeValue, signal) => resolve({ code: codeValue, signal }));
  });
  assert.deepEqual(exit, { code: 0, signal: null });
  assert.match(stdout, /BRIDGE_ALIVE sessions=1 successorAborted=false/);
  assert.doesNotMatch(stderr, /FATAL unhandledRejection/);
  assert.doesNotMatch(stderr, /restarting isolated sidecar/);
  assert.match(stderr, /expected SDK cancellation/);
});

test("a global cancellation ticket cannot mask an unrelated live SDK failure", async () => {
  const moduleUrl = new URL("./cursor-agent-bridge.mjs", import.meta.url).href;
  const code = `
    const bridge = await import(${JSON.stringify(moduleUrl)});
    bridge.noteExpectedSdkAbort("already-cancelled", 3);
    bridge.sessions.set("unrelated-live", { run: {}, activeRes: {}, done: false, runEpoch: 1 });
    const closed = Object.assign(new Error("WritableIterable is closed"), { name: "WriteIterableClosedError" });
    Promise.reject(closed);
    setTimeout(() => process.exit(9), 1000);
  `;
  const child = spawn(process.execPath, ["--input-type=module", "--eval", code], {
    cwd: path.dirname(new URL(import.meta.url).pathname),
    env: { ...process.env, CURSOR_AGENT_STATE_ROOT: TEST_STATE_ROOT },
    stdio: ["ignore", "pipe", "pipe"],
  });
  let stderr = "";
  child.stderr.on("data", (chunk) => { stderr += chunk; });
  const exit = await new Promise((resolve, reject) => {
    child.once("error", reject);
    child.once("exit", (codeValue, signal) => resolve({ code: codeValue, signal }));
  });
  assert.deepEqual(exit, { code: 1, signal: null });
  assert.match(stderr, /ambiguous SDK input-stream closure; restarting isolated sidecar/);
});

test("an unticketed identity-less SDK closure restarts only the sidecar instead of stranding a run", async () => {
  const moduleUrl = new URL("./cursor-agent-bridge.mjs", import.meta.url).href;
  const code = `
    await import(${JSON.stringify(moduleUrl)});
    const closed = Object.assign(new Error("WritableIterable is closed"), { name: "WriteIterableClosedError" });
    Promise.reject(closed);
    setTimeout(() => process.exit(9), 1000);
  `;
  const child = spawn(process.execPath, ["--input-type=module", "--eval", code], {
    cwd: path.dirname(new URL(import.meta.url).pathname),
    env: { ...process.env, CURSOR_AGENT_STATE_ROOT: TEST_STATE_ROOT },
    stdio: ["ignore", "pipe", "pipe"],
  });
  let stderr = "";
  child.stderr.on("data", (chunk) => { stderr += chunk; });
  const exit = await new Promise((resolve, reject) => {
    child.once("error", reject);
    child.once("exit", (codeValue, signal) => resolve({ code: codeValue, signal }));
  });
  assert.deepEqual(exit, { code: 1, signal: null });
  assert.match(stderr, /unattributed SDK input-stream closure; restarting isolated sidecar/);
});

test("concurrent Session.cancel calls share one SDK teardown and upgrade notify", async () => {
  const session = new Session("single-flight-cancel", "key");
  let runCancels = 0;
  let agentCloses = 0;
  let releaseRunCancel;
  const runCancelGate = new Promise((resolve) => { releaseRunCancel = resolve; });
  session.run = {
    async cancel() {
      runCancels++;
      await runCancelGate;
    },
  };
  session.agent = { async close() { agentCloses++; } };
  const logicalDone = session.whenLogicalDone();

  const internal = session.cancel({ notify: false, terminalReason: "interrupted", detail: "internal replacement" });
  await waitFor(() => runCancels === 1, "first SDK cancel");
  const external = session.cancel({ notify: true, terminalReason: "client_cancelled", detail: "concurrent close" });
  releaseRunCancel();
  await Promise.all([internal, external, logicalDone]);

  assert.equal(runCancels, 1);
  assert.equal(agentCloses, 1);
  assert.equal(session.run, null);
  assert.equal(session.agent, null);
  assert.equal(consumeExpectedSdkLifecycleClosure(), true);
});

test("Session.cancel emits exactly one terminal receipt before clearing a live response", async () => {
  for (const [terminalReason, retryable] of [
    [TerminalReason.INTERRUPTED, false],
    [TerminalReason.SHUTDOWN, true],
  ]) {
    const session = new Session(`terminal-before-cancel-${terminalReason}`, "key");
    const response = new MockResponse();
    response.writeHead(200, { "Content-Type": "text/event-stream" });
    session.beginResponse(response);
    session.turnToken = 1;
    session.run = { async cancel() {} };
    session.agent = { async close() {} };

    await session.cancel({ terminalReason, detail: `cancel ${terminalReason}` });
    await session.finishResponse();

    const terminalFrames = response.text().match(/\"type\":\"turn_end\"/g) || [];
    assert.equal(terminalFrames.length, 1);
    assert.match(response.text(), /session_cancellation_terminal/);
    assert.match(response.text(), new RegExp(`\"retryable\":${retryable}`));
    assert.ok(response.text().indexOf("turn_end") < response.text().indexOf("[DONE]"));
  }
});

test("an existing terminal prevents cancellation cleanup from emitting a duplicate", async () => {
  const session = new Session("terminal-before-double-cancel", "key");
  const response = new MockResponse();
  response.writeHead(200, { "Content-Type": "text/event-stream" });
  session.beginResponse(response);
  session.turnToken = 1;
  session.run = { async cancel() {} };
  session.agent = { async close() {} };
  session.sse({ type: "turn_end", stop_reason: "error", error: "already terminal" });

  await session.cancel({ terminalReason: TerminalReason.SHUTDOWN, detail: "must not duplicate" });
  await session.finishResponse();

  assert.equal((response.text().match(/\"type\":\"turn_end\"/g) || []).length, 1);
  assert.doesNotMatch(response.text(), /session_cancellation_terminal/);
});

test("request-local post-header failures end SSE with a typed terminal instead of a false 200 EOF", async () => {
  const response = new MockResponse();
  const ok = await handleHttpRequestSafely(request(), response, async (_req, res) => {
    res.writeHead(200, { "Content-Type": "text/event-stream" });
    res.write('data: {"type":"session","sessionId":"post-header"}\n\n');
    throw new Error("after headers");
  });

  assert.equal(ok, false);
  assert.equal(response.ended, true);
  assert.match(response.text(), /request_failure_after_headers/);
  assert.match(response.text(), /\"stop_reason\":\"error\"/);
  assert.ok(response.text().indexOf("turn_end") < response.text().indexOf("[DONE]"));
});

test("a wedged SDK cancellation cleanup triggers a bounded isolated sidecar restart", async () => {
  const moduleUrl = new URL("./cursor-agent-bridge.mjs", import.meta.url).href;
  const code = `
    const bridge = await import(${JSON.stringify(moduleUrl)});
    const session = new bridge.Session("wedged-cancel", "key");
    session.run = { async cancel() { await new Promise(() => {}); } };
    bridge.sessions.set(session.id, session);
    session.cancel().catch(() => {});
    setTimeout(() => process.exit(9), 2000);
  `;
  const child = spawn(process.execPath, ["--input-type=module", "--eval", code], {
    cwd: path.dirname(new URL(import.meta.url).pathname),
    env: { ...process.env, CURSOR_AGENT_STATE_ROOT: TEST_STATE_ROOT, CURSOR_AGENT_CANCEL_CLEANUP_MS: "100" },
    stdio: ["ignore", "pipe", "pipe"],
  });
  let stderr = "";
  child.stderr.on("data", (chunk) => { stderr += chunk; });
  const exit = await new Promise((resolve, reject) => {
    child.once("error", reject);
    child.once("exit", (codeValue, signal) => resolve({ code: codeValue, signal }));
  });
  assert.deepEqual(exit, { code: 1, signal: null });
  assert.match(stderr, /SDK cancellation cleanup abandoned; restarting isolated sidecar/);
});

test("planned shutdown has one global deadline even when many session cancels never settle", async () => {
  let startedTasks = 0;
  const taskStartedAt = Date.now();
  await runBoundedShutdownTasks(
    Array.from({ length: 64 }, () => () => {
      startedTasks++;
      return new Promise(() => {});
    }),
    Date.now() + 50,
    8,
  );
  const taskElapsed = Date.now() - taskStartedAt;
  assert.equal(startedTasks, 8, "the bounded pool must start only its configured concurrency");
  assert.ok(taskElapsed >= 40 && taskElapsed < 500, `bounded cleanup elapsed ${taskElapsed}ms`);

  const childStateRoot = mkdtempSync(path.join(tmpdir(), "cursor-bridge-shutdown-"));
  const moduleURL = new URL("./cursor-agent-bridge.mjs", import.meta.url).href;
  const source = `
    const bridge = await import(${JSON.stringify(moduleURL)});
    for (let index = 0; index < 64; index++) {
      const session = new bridge.Session("shutdown-wedge-" + index, "shutdown-key");
      session.run = {};
      session.cancel = () => new Promise(() => {});
      bridge.sessions.set(session.id, session);
    }
    process.kill(process.pid, "SIGTERM");
  `;
  const startedAt = Date.now();
  const child = spawn(process.execPath, ["--input-type=module", "-e", source], {
    env: {
      ...process.env,
      CURSOR_AGENT_STATE_ROOT: childStateRoot,
      CURSOR_AGENT_DRAIN_MS: "0",
      CURSOR_AGENT_SHUTDOWN_MAX_MS: "1500",
      CURSOR_AGENT_SHUTDOWN_CANCEL_CONCURRENCY: "8",
    },
    stdio: ["ignore", "pipe", "pipe"],
  });
  const exit = await new Promise((resolve, reject) => {
    child.once("error", reject);
    child.once("exit", (codeValue, signal) => resolve({ code: codeValue, signal }));
  });
  const elapsed = Date.now() - startedAt;
  rmSync(childStateRoot, { recursive: true, force: true });
  assert.deepEqual(exit, { code: 0, signal: null });
  assert.ok(elapsed < 2600, `planned shutdown exceeded its global bound: ${elapsed}ms`);
});

test("standing reminders precede an exact authoritative user suffix", () => {
  const user = "why are you ignoring my SSH request?";
  const framed = appendRulesReminder(user, "Re-read the tool contract.");
  assert.ok(framed.indexOf("Re-read the tool contract.") < framed.indexOf(user));
  assert.ok(framed.endsWith(user));
  assert.match(framed, /CURRENT USER MESSAGE/);
  assert.match(appendRulesReminder("continue"), /continuation or clarification keeps relevant unfinished work/);
});

test("system context uses stable generic blocks for same, append, replacement, and removal", () => {
  const block = (char, text) => ({ id: `csb1_${char.repeat(40)}`, text });
  const base = block("a", "base policy");
  const suffix = block("b", "new policy");
  const replacement = block("c", "replacement policy");
  const session = new Session("system-block-plan");
  session.seeded = true;
  session.seededSystem = base.text;
  session.seededSystemBlockIds = [base.id];

  assert.deepEqual(systemContextPlan(session, {
    system: base.text,
    systemBlocks: [base],
  }), { kind: "same", blocks: [base], ids: [base.id], text: "" });
  assert.equal(systemContextPlan(session, {
    system: "changed text under a reused opaque id",
    systemBlocks: [{ ...base, text: "changed text under a reused opaque id" }],
  }).kind, "replace");

  const appended = systemContextPlan(session, {
    system: `${base.text}\n\n${suffix.text}`,
    systemBlocks: [base, suffix],
  });
  assert.equal(appended.kind, "append");
  assert.equal(appended.text, suffix.text);
  assert.deepEqual(appended.ids, [base.id, suffix.id]);

  assert.equal(systemContextPlan(session, {
    system: replacement.text,
    systemBlocks: [replacement],
  }).kind, "replace");
  assert.equal(systemContextPlan(session, { system: "", systemBlocks: [] }).kind, "replace");
  assert.equal(systemContextPlan(session, { system: "opaque legacy aggregate" }).kind, "legacy_update");
  assert.throws(
    () => normalizeSystemBlocks({ system: "does not match", systemBlocks: [base] }),
    (error) => error instanceof ToolRoundError && error.code === "invalid_system_blocks" && error.status === 422,
  );
});

test("a warm agent receives only the new system-block suffix before the current user", async () => {
  const base = { id: `csb1_${"e".repeat(40)}`, text: "base policy must not repeat" };
  const suffix = { id: `csb1_${"f".repeat(40)}`, text: "new append-only policy" };
  const session = new Session("system-block-delta-send", "system-block-key");
  session.clientEnv = { workspaceUnknown: true };
  session.seeded = true;
  session.seededSystem = base.text;
  session.seededSystemBlockIds = [base.id];
  session.model = "cursor-grok-4.5";
  const sent = [];
  const fakeAgent = {
    async send(message) {
      sent.push(typeof message === "string" ? message : message.text);
      return {
        async wait() { return { status: "finished", result: "done" }; },
        async cancel() {},
      };
    },
    async close() {},
  };
  session.agent = fakeAgent;
  sessions.set(session.id, session);
  try {
    const response = new MockResponse();
    const input = {
      type: "user",
      text: "authoritative current request",
      clientMessageId: "ccm2_system_block_delta",
      system: `${base.text}\n\n${suffix.text}`,
      systemBlocks: [base, suffix],
      history: "USER: earlier task\nASSISTANT: earlier answer",
    };
    assert.equal(systemContextPlan(session, input).kind, "append");
    await handleTurn(request(), response, neutralBody({
      sessionId: session.id,
      model: "cursor-grok-4.5",
      input,
    }), "system-block-key");
    await waitFor(() => sent.length === 1 && response.ended, "system block delta completion");
    assert.equal(sessions.get(session.id), session);
    assert.equal(session.agent, fakeAgent);
    assert.equal(sent.length, 1, response.text());
    assert.match(sent[0], /Additional system instructions:\nnew append-only policy/);
    assert.doesNotMatch(sent[0], /base policy must not repeat/);
    assert.ok(sent[0].endsWith("authoritative current request"), sent[0]);
    assert.match(response.text(), /"stop_reason":"end_turn"/);
  } finally {
    sessions.delete(session.id);
  }
});

test("ToolRound persists bounded recovery context when it opens, before any callback handoff", () => {
  const session = new Session("round-open-recovery-context");
  const systemBlock = { id: `csb1_${"d".repeat(40)}`, text: "standing policy" };
  session.roundRecoveryContext = {
    system: systemBlock.text,
    systemBlocks: [systemBlock],
    history: "USER: earlier task\nASSISTANT: earlier answer",
    historyFingerprint: "fp_round_open",
    currentUser: "the authoritative current request",
    toolInventoryEpoch: "cti1_round_open",
    images: [{ data: "aGVsbG8=", mimeType: "image/png" }],
  };
  const round = session.ensureToolRound();
  const loaded = ToolRound.load(createRoundInfrastructure(TEST_STATE_ROOT).journal,
    createRoundInfrastructure(TEST_STATE_ROOT).codec, round.route);
  assert.deepEqual(loaded.recoveryContext, session.roundRecoveryContext);
  round.terminalize(TerminalReason.CLIENT_CANCELLED, "test cleanup");
});

test("round-open context recovers a first-turn tool result without later client history", async () => {
  const { session } = seedSession("round-open-first-turn-recovery", "round-open-key");
  const systemBlock = { id: `csb1_${"1".repeat(40)}`, text: "standing policy" };
  session.roundRecoveryContext = {
    system: systemBlock.text,
    systemBlocks: [systemBlock],
    history: "",
    historyFingerprint: "fp_first_turn",
    currentUser: "perform the first-turn task",
    toolInventoryEpoch: "cti1_first_turn",
    images: [{ data: "aGVsbG8=", mimeType: "image/png" }],
  };
  const opened = await openTool(session);
  const result = { toolCallId: opened.call.wireId, content: "durable tool output", isError: false };
  opened.round.applyResults([result]);
  await opened.promise;
  const recovered = buildRestartRecoveryInput(opened.round, { type: "tool_results", results: [result] });
  assert.ok(recovered, "durable round-open current user context should make recovery possible");
  assert.equal(recovered.text, "perform the first-turn task");
  assert.equal(recovered.system, systemBlock.text);
  assert.deepEqual(recovered.systemBlocks, [systemBlock]);
  assert.deepEqual(recovered.images, [{ data: "aGVsbG8=", mimeType: "image/png" }]);
  assert.match(recovered.recoveryContext, /durable tool output/);
});

test("an attributed poisoned platform terminalizes only its affected live sessions", async () => {
  const affected = new Session("poisoned-platform-session", "poison-key");
  const unrelated = new Session("healthy-platform-session", "healthy-key");
  const response = new MockResponse();
  affected.beginResponse(response);
  affected.run = { async cancel() {} };
  affected.agent = { async close() {} };
  sessions.set(affected.id, affected);
  sessions.set(unrelated.id, unrelated);
  try {
    const count = terminalizePoisonedPlatformSessions(
      keyHash("poison-key"),
      new Error("upstream authentication transport was rejected"),
    );
    await new Promise((resolve) => setImmediate(resolve));
    assert.equal(count, 1);
    assert.equal(affected.done, true);
    assert.match(response.text(), /"stop_reason":"error"/);
    assert.match(response.text(), /authentication transport was rejected/);
    assert.equal(unrelated.done, false);
  } finally {
    sessions.delete(affected.id);
    sessions.delete(unrelated.id);
  }
});

test("workspace identity is advisory and unknown context never invents a server path", () => {
  const context = headlessRequestContext({ clientEnv: { workspaceUnknown: true }, advertiseForGating: () => [] })
    .__ccJson.success.requestContext;
  const env = context.env;
  assert.deepEqual(env.workspacePaths, []);
  assert.equal(env.processWorkingDirectory, undefined);
  assert.equal(env.projectFolder, undefined);
  assert.equal(composerWorkspaceCwd(null), "");
  assert.equal(composerWorkspaceCwd({ workspaceUnknown: true }), "");
  assert.equal(toolManifestRule(advertised(), ""), null);
  assert.equal(context.mcpMetaToolOptions.enabled, false);
  assert.doesNotMatch(JSON.stringify(env), /\/workspace|\/app/);
});

test("known workspace context is projected without changing its path", () => {
  const clientEnv = { workspacePaths: ["/home/me/repo"], processWorkingDirectory: "/home/me/repo", shell: "zsh" };
  const env = headlessRequestContext({ clientEnv, advertiseForGating: () => [] }).__ccJson.success.requestContext.env;
  assert.deepEqual(env.workspacePaths, ["/home/me/repo"]);
  assert.equal(env.processWorkingDirectory, "/home/me/repo");
  assert.equal(env.shell, "zsh");
  assert.equal(composerWorkspaceCwd(clientEnv), "/home/me/repo");
});

test("/agent/turn accepts missing workspace as neutral but refuses continuation on the wrong endpoint", async () => {
  const res = new MockResponse();
  await handleTurn(request(), res, { sessionId: "headerless", input: { type: "tool_results", results: [{ toolCallId: "x", content: "y" }] } }, "key");
  assert.equal(res.status, 400);
  assert.match(res.text(), /\/agent\/continue/);
  assert.notEqual(res.status, 422);
});

test("/agent/turn rejects null and array JSON bodies without throwing", async () => {
  for (const body of [null, []]) {
    const res = new MockResponse();
    await handleTurn(request(), res, body, "key");
    assert.equal(res.status, 400);
    assert.match(res.text(), /JSON object/);
  }
});

test("malformed supplied workspace degrades to neutral context instead of rejecting the turn", async () => {
  const res = new MockResponse();
  await handleTurn(request(), res, { sessionId: "bad-workspace", clientEnv: { workspacePaths: [] }, input: { type: "tool_results", results: [{ toolCallId: "x" }] } }, "key");
  assert.equal(res.status, 400);
  assert.notEqual(res.status, 422);
  assert.deepEqual(sessions.get("bad-workspace"), undefined);
});

test("an invalid advertised schema is quarantined without rejecting a new turn or widening it", async () => {
  const cursorKey = "invalid-schema-key";
  const agent = {
    async send() {
      return {
        async wait() { return { status: "finished", result: "schema drift accommodated" }; },
        async cancel() {},
      };
    },
    async close() {},
  };
  platforms.set(keyHash(cursorKey), {
    promise: Promise.resolve({
      async resumeAgent() { throw new Error("agent not found"); },
      async createAgent() { return agent; },
      async getAgentMessages() { return []; },
    }),
    stateRoot: TEST_STATE_ROOT,
    lastUsed: Date.now(),
    fp: keyFingerprint(cursorKey),
  });
  const res = new MockResponse();
  await handleTurn(request(), res, neutralBody({
    sessionId: "invalid-tool-schema",
    input: { type: "user", text: "hello", clientMessageId: "ccm2_invalid_schema" },
    tools: [{ name: "Broken", inputSchema: { type: "not-a-json-schema-type" } }],
  }), cursorKey);
  await waitFor(() => res.ended, "schema accommodation response");
  assert.equal(res.status, 200);
  assert.match(res.text(), /schema drift accommodated/);
  assert.deepEqual(sessions.get("invalid-tool-schema").advertise, []);
});

test("an all-invalid dynamic inventory preserves the established last-known-good registry", async () => {
  const session = new Session("invalid-dynamic-schema", "key");
  session.clientEnv = { workspaceUnknown: true };
  session.toolRegistry.replace(advertised("KnownGood"));
  session.seeded = true;
  let sends = 0;
  session.agent = {
    async send() {
      sends++;
      return {
        async wait() { return { status: "finished", result: "continued with adapted tools" }; },
        async cancel() {},
      };
    },
    async close() {},
  };
  sessions.set(session.id, session);
  const res = new MockResponse();
  await handleTurn(request(), res, neutralBody({
    sessionId: session.id,
    input: { type: "user", text: "continue despite one bad tool update", clientMessageId: "ccm2_bad_inventory" },
    tools: [{ name: "Broken", inputSchema: { type: "not-a-json-schema-type" } }],
  }), "key");
  await waitFor(() => res.ended, "last-known-good inventory response");
  assert.equal(res.status, 200);
  assert.match(res.text(), /continued with adapted tools/);
  assert.deepEqual(session.advertise.map((tool) => tool.name), ["KnownGood"]);
  assert.equal(sends, 1);
});

test("mixed tool inventories keep valid structural schemas and quarantine impossible descriptors", () => {
  const registry = prepareAdvertisedToolRegistry({ tools: [
    {
      name: "Search",
      inputSchema: {
        type: "object",
        properties: { repo_path: { type: "string" }, project: { type: "string" } },
        required: ["repo_path", "project"],
        additionalProperties: false,
      },
    },
    { name: "Never", inputSchema: false },
    { name: "Broken", inputSchema: { type: "not-a-json-schema-type" } },
  ] });
  assert.deepEqual(registry.all().map((tool) => tool.name), ["Search"]);
  assert.deepEqual(registry.find("Search").inputSchema.required, ["repo_path", "project"]);
  assert.equal(registry.rejectedCount, 2);
  const normalized = registry.normalize("Search", {});
  const failure = registry.validate("Search", normalized.value);
  assert.deepEqual(failure.structuredContent.errors.map((error) => error.path).sort(), ["project", "repo_path"]);
});

test("unknown workspace never leaks the SDK isolation directory through shell result metadata", () => {
  const unary = CC_CASES.shellArgs.buildResult("ok", { command: "pwd" }, false, { cwd: "" });
  const stream = CC_CASES.shellStreamArgs.buildChunks("ok", false, { cwd: "" });
  assert.equal(unary.success.workingDirectory, "");
  assert.equal(stream.at(-1).exit.cwd, "");
  assert.doesNotMatch(JSON.stringify({ unary, stream }), /\.cursor-agent-store|\/app|\/workspace/);
});

test("advertised tools have one normalized registry and duplicate names fail closed", () => {
  const registry = new AdvertisedToolRegistry();
  registry.replace([{ name: "A" }, { toolName: "B", inputSchema: { type: "object" } }]);
  assert.deepEqual(registry.all().map((tool) => tool.name), ["A", "B"]);
  assert.deepEqual(registry.find("A").inputSchema, { type: "object" });
  assert.throws(() => registry.replace([{ name: "A" }, { toolName: "A" }]), /appears more than once/);
  assert.throws(() => registry.replace([{}]), /missing a string name/);
  assert.throws(() => registry.replace([{ name: "Broken", inputSchema: { type: "definitely-not-json-schema" } }]), /invalid inputSchema/);
  assert.throws(() => registry.replace([{ name: "Async", inputSchema: { $async: true, type: "object" } }]), /asynchronous JSON Schema/);
});

test("private descriptor aliases survive body refresh, route explicitly, and never reach the SDK inventory", () => {
  const registry = prepareAdvertisedToolRegistry({ tools: [{
    name: "RunCommand",
    description: "Run a command",
    aliases: ["shell", "terminal", "shell"],
    inputSchema: {
      type: "object",
      properties: { command: { type: "string" } },
      required: ["command"],
      additionalProperties: false,
    },
  }] });
  const session = new Session("private-aliases", "key");
  session.toolRegistry = registry;
  assert.deepEqual(registry.find("RunCommand").aliases, ["shell", "terminal"]);
  assert.equal(session.reconcileToolName("shell"), "RunCommand");
  assert.equal(session.reconcileToolName("terminal"), "RunCommand");
  assert.equal(Object.hasOwn(sdkAdvertisedTools(session)[0], "aliases"), false);
  const malformedAliases = prepareAdvertisedToolRegistry({
    tools: [{ name: "Bad", aliases: "shell", inputSchema: { type: "object" } }],
  });
  assert.equal(Object.hasOwn(malformedAliases.find("Bad"), "aliases"), false);
});

test("tool inventory refresh replaces live gating, including an explicit empty clear", () => {
  const session = new Session("dynamic-tools", "key");
  refreshSessionFromBody(session, { tools: [{ name: "A", inputSchema: { type: "object" } }], toolChoice: "auto" });
  session.activeAdvertise = effectiveAdvertise(session.advertise, "auto");
  assert.deepEqual(session.advertiseForGating().map((tool) => tool.name), ["A"]);

  refreshSessionFromBody(session, { tools: [{ name: "B", inputSchema: { type: "object" } }], toolChoice: "auto" });
  assert.deepEqual(session.advertiseForGating().map((tool) => tool.name), ["B"]);

  refreshSessionFromBody(session, { tools: [], toolChoice: "none" });
  assert.deepEqual(session.advertise, []);
  assert.deepEqual(session.advertiseForGating(), []);

  refreshSessionFromBody(session, {
    tools: [{ name: "B", inputSchema: { type: "object" } }, { name: "C", inputSchema: { type: "object" } }],
    toolChoice: "specific:B",
  });
  assert.deepEqual(session.advertiseForGating().map((tool) => tool.name), ["B"]);
  refreshSessionFromBody(session, {
    tools: [{ name: "B", inputSchema: { type: "object" } }, { name: "C", inputSchema: { type: "object" } }],
  });
  assert.deepEqual(session.advertiseForGating().map((tool) => tool.name), ["B", "C"]);
});

test("client-tool contract v2 hashes the one Go-emitted snapshot and rejects ambiguous representations", () => {
  const body = authoritativeToolsBody([]);
  const registry = prepareAdvertisedToolRegistry(body);
  assert.deepEqual(registry.all(), []);
  assert.equal(Object.hasOwn(body, "tools"), false, "preparation must not create a second inventory representation");
  assert.doesNotThrow(() => prepareAdvertisedToolRegistry(body), "preparation must be idempotent");
  assert.throws(
    () => prepareAdvertisedToolRegistry({
      ...body,
      toolInventoryEpoch: "cti1_00000000000000000000000000000000",
    }),
    /does not match/,
  );
  assert.throws(
    () => prepareAdvertisedToolRegistry({ ...body, toolsAuthoritative: false }),
    /toolsAuthoritative:true/,
  );
  assert.throws(() => prepareAdvertisedToolRegistry({ ...body, tools: [] }), /exactly one authoritative/);
  assert.throws(() => prepareAdvertisedToolRegistry(authoritativeRawToolsBody("{")), /not valid JSON/);
  assert.throws(() => prepareAdvertisedToolRegistry(authoritativeRawToolsBody("{}")), /must encode a tools array/);

  // This is a literal Go encoding/json snapshot. It intentionally contains
  // HTML escaping, U+2028 escaping, an integer-like object key, -0, and an
  // integer above JavaScript's exact range. Node hashes the original bytes
  // before JSON.parse, so none of those legal representation differences can
  // cause a false epoch mismatch/422.
  const goSnapshot = String.raw`[{"description":"\u003c\u0026\u003e\u2028","inputSchema":{"properties":{"0":{"minimum":-0,"type":"number"},"huge":{"maximum":9007199254740993,"type":"integer"}},"type":"object"},"name":"T"}]`;
  const crossLanguageBody = authoritativeRawToolsBody(goSnapshot);
  assert.equal(crossLanguageBody.toolInventoryEpoch, "cti1_2bb750689a0806d0a1f5b6e984f6a7d0");
  assert.equal(prepareAdvertisedToolRegistry(crossLanguageBody).all()[0].name, "T");

  const compact = authoritativeRawToolsBody(`[{"name":"Equivalent","inputSchema":{"type":"object","properties":{"x":{"type":"string"}}}}]`);
  const reordered = authoritativeRawToolsBody(`[
    { "inputSchema": { "properties": { "x": { "type": "string" } }, "type": "object" }, "name": "Equivalent" }
  ]`);
  assert.notEqual(compact.toolInventoryEpoch, reordered.toolInventoryEpoch, "the epoch identifies bytes, not a guessed semantic canonical form");
  assert.deepEqual(prepareAdvertisedToolRegistry(compact).all(), prepareAdvertisedToolRegistry(reordered).all(),
    "equivalent valid snapshots are accepted even when their whitespace/key order differ");
});

test("bounded request decoding preserves UTF-8 across every network chunk boundary", async () => {
  const raw = JSON.stringify(authoritativeToolsBody([{
    name: "UnicodeTool",
    description: "é 中 😀 e\u0301",
    inputSchema: { type: "object", properties: { "clé😀": { type: "string" } } },
  }]));
  const encoded = Buffer.from(raw, "utf8");
  for (let split = 1; split < encoded.length; split++) {
    const decoded = await readBodyBounded(Readable.from([encoded.subarray(0, split), encoded.subarray(split)]));
    assert.equal(decoded, raw, `UTF-8 body changed at byte split ${split}`);
    const body = JSON.parse(decoded);
    assert.doesNotThrow(() => prepareAdvertisedToolRegistry(body));
  }
  await assert.rejects(
    readBodyBounded(Readable.from([Buffer.from([0x7b, 0x22, 0xc3]), Buffer.from([0x28, 0x22, 0x7d])])),
    /encoded data was not valid|UTF-8/i,
  );
});

test("shared SSE frame limit counts UTF-8 bytes and rejects before Go scanner overflow", () => {
  assert.equal(sseFrameSizeError("é", 2), null);
  assert.match(sseFrameSizeError("é", 1).message, /2 bytes/);
  assert.equal(sseFrameSizeError("abc", 3), null);
  assert.match(sseFrameSizeError("abc", 2).message, /shared 2-byte limit/);
});

test("MCP reconnect snapshots replace 20 tools with 28 tools and never retain stale entries", () => {
  const session = new Session("dynamic-mcp-reconnect", "key");
  const inventory = (count, generation) => Array.from({ length: count }, (_, index) => ({
    name: `mcp__memory_${generation}__tool_${index}`,
    description: `generation ${generation} tool ${index}`,
    inputSchema: {
      type: "object",
      properties: {
        repo_path: { type: "string" },
        project: { type: ["string", "null"], default: null },
        options: { type: "array", items: { type: "integer", minimum: -0 } },
      },
      required: ["repo_path"],
      additionalProperties: false,
    },
  }));

  let priorEpoch = null;
  for (const [count, generation] of [[20, "initial"], [28, "reconnected"], [7, "reduced"], [0, "disabled"]]) {
    const body = authoritativeToolsBody(inventory(count, generation), { toolChoice: "auto" });
    const prepared = prepareAdvertisedToolRegistry(body);
    refreshSessionFromBody(session, body, prepared);
    assert.equal(session.advertise.length, count);
    assert.equal(session.toolInventoryEpoch, body.toolInventoryEpoch);
    if (priorEpoch != null) assert.notEqual(session.toolInventoryEpoch, priorEpoch);
    assert.ok(session.advertise.every((tool) => tool.name.includes(generation)), "a replacement snapshot must not merge stale tools");
    priorEpoch = session.toolInventoryEpoch;
  }
});

test("authoritative inventory may add, remove, rename, and change MCP schemas while an older signed call completes", async () => {
  const { session } = seedSession("dynamic-contract-v2", "key", []);
  const initial = authoritativeToolsBody([{
    name: "MemorySearch",
    inputSchema: { type: "object", properties: { query: { type: "string" } }, required: ["query"] },
  }], { toolChoice: "auto" });
  refreshSessionFromBody(session, initial, prepareAdvertisedToolRegistry(initial));
  session.activeAdvertise = effectiveAdvertise(session.advertise, "auto");
  const firstEpoch = session.toolInventoryEpoch;
  const opened = await openTool(session, { name: "MemorySearch", input: { query: "old schema" } });

  const changed = authoritativeToolsBody([{
    name: "GraphSearch",
    aliases: ["MemorySearch"],
    inputSchema: { type: "object", properties: { repo_path: { type: "string" }, project: { type: "string" } }, required: ["repo_path", "project"] },
  }, {
    name: "Todo",
    inputSchema: { type: "object", properties: { op: { type: "string" } }, required: ["op"] },
  }], { toolChoice: "auto" });
  const continuation = new MockResponse();
  await handleContinue(request(), continuation, continuationBody([{
    toolCallId: opened.call.wireId,
    content: "old call completed",
    isError: false,
  }], {}, changed), "key");

  assert.equal(continuation.status, 200);
  assert.notEqual(session.toolInventoryEpoch, firstEpoch);
  assert.deepEqual(session.advertiseForGating().map((tool) => tool.name), ["GraphSearch", "Todo"]);
  assert.equal((await opened.promise).content, "old call completed", "inventory refresh must not invalidate an already-issued signed call");
  assert.equal(session.reconcileToolName("MemorySearch"), "GraphSearch", "renames may retain a private compatibility alias");

  const removed = authoritativeToolsBody([{ name: "Todo", inputSchema: { type: "object" } }], { toolChoice: "auto" });
  refreshSessionFromBody(session, removed, prepareAdvertisedToolRegistry(removed));
  assert.deepEqual(session.advertiseForGating().map((tool) => tool.name), ["Todo"]);
  assert.equal(session.reconcileToolName("MemorySearch"), null, "removed tools must not survive in stale session state");
});

test("partial continuations cannot reorder tool inventory; refresh occurs only at the logical round boundary", async () => {
  const initial = authoritativeToolsBody(advertised("Lookup"), { toolChoice: "auto" });
  const { session } = seedSession("inventory-boundary", "key", []);
  refreshSessionFromBody(session, initial, prepareAdvertisedToolRegistry(initial));
  session.activeAdvertise = effectiveAdvertise(session.advertise, "auto");
  const first = await openTool(session, { rawId: "boundary-first", awaiting: false });
  const secondPromise = session.openClientTool({
    source: "test",
    rawToolCallId: "boundary-second",
    name: "Lookup",
    input: { q: "second" },
    resultAdapter: (value) => value,
  });
  secondPromise.catch(() => {});
  await waitFor(() => first.round.fifo.length === 2 && first.round.calls.get(first.round.fifo[1]).handedAt != null,
    "second boundary call handoff");
  if (session.flushTimer) { clearTimeout(session.flushTimer); session.flushTimer = null; }
  const second = first.round.calls.get(first.round.fifo[1]);
  first.round.markAwaitingResults();

  const premature = authoritativeToolsBody(advertised("PrematureTool"), { toolChoice: "auto" });
  const partial = new MockResponse();
  await handleContinue(request(), partial, continuationBody([
    { toolCallId: first.call.wireId, content: "first" },
  ], {}, premature), "key");
  assert.equal(partial.status, 200);
  assert.deepEqual(session.advertise.map((tool) => tool.name), ["Lookup"],
    "a partial receipt must not mutate the still-open logical turn contract");

  const finalInventory = authoritativeToolsBody(advertised("FinalTool"), { toolChoice: "auto" });
  const final = new MockResponse();
  await handleContinue(request(), final, continuationBody([
    { toolCallId: second.wireId, content: "second" },
  ], {}, finalInventory), "key");
  assert.equal(final.status, 200);
  assert.deepEqual(session.advertise.map((tool) => tool.name), ["FinalTool"]);
  assert.equal((await first.promise).content, "first");
  assert.equal((await secondPromise).content, "second");
});

test("boolean false tool schemas are never widened into model-callable permissive contracts", () => {
  const prepared = prepareAdvertisedToolRegistry({
    tools: [{ name: "Never", inputSchema: false }],
  });
  assert.equal(prepared.find("Never"), null);
  assert.equal(prepared.rejectedCount, 1);
  const registry = new AdvertisedToolRegistry();
  registry.replace([{ name: "Never", inputSchema: false }]);
  const normalized = registry.normalize("Never", {});
  const failure = registry.validate("Never", normalized.value);
  assert.equal(failure.structuredContent.executed, false);
});

test("boolean true tool schemas remain valid unconstrained client tools", () => {
  const registry = prepareAdvertisedToolRegistry({
    tools: [{ name: "Anything", description: "accepts any object arguments", inputSchema: true }],
  });
  assert.deepEqual(registry.all().map((tool) => tool.name), ["Anything"]);
  assert.deepEqual(registry.find("Anything").inputSchema, {
    type: "object",
    additionalProperties: true,
  });
});

test("advertised schema validation honors declared modern JSON Schema dialects", () => {
  const registry = new AdvertisedToolRegistry();
  registry.replace([{
    name: "Modern",
    inputSchema: {
      $schema: "https://json-schema.org/draft/2020-12/schema",
      type: "object",
      properties: {
        pair: {
          type: "array",
          prefixItems: [{ type: "string" }, { type: "number" }],
          items: false,
        },
      },
      required: ["pair"],
    },
  }]);
  assert.equal(registry.validate("Modern", { pair: ["x", 1] }), null);
  assert.equal(registry.validate("Modern", { pair: ["x", "wrong"] }).structuredContent.errors[0].path, "pair.1");
});

test("draft-04 schemas validate and unresolved external refs defer to the authoritative harness", () => {
  const draft04 = new AdvertisedToolRegistry();
  draft04.replace([{
    name: "Draft04",
    inputSchema: {
      $schema: "http://json-schema.org/draft-04/schema#",
      type: "object",
      properties: { count: { type: "number", minimum: 0, exclusiveMinimum: true } },
      required: ["count"],
      additionalProperties: false,
    },
  }]);
  assert.equal(draft04.validate("Draft04", { count: 1 }), null);
  const zeroFailure = draft04.validate("Draft04", { count: 0 });
  assert.equal(zeroFailure.structuredContent.errors[0].path, "count");
  assert.match(zeroFailure.structuredContent.errors[0].message, /must be > 0/);

  const remote = new AdvertisedToolRegistry();
  remote.replace([{
    name: "RemoteRef",
    inputSchema: {
      $schema: "https://json-schema.org/draft/2020-12/schema",
      $ref: "https://schemas.example.invalid/client-tool-input.json",
    },
  }]);
  assert.equal(remote.validate("RemoteRef", { client_specific: true }), null,
    "an unresolved remote ref must reach the client harness instead of failing registration");

  assert.throws(
    () => new AdvertisedToolRegistry().replace([{ name: "InvalidSchema", inputSchema: { type: 7 } }]),
    /invalid inputSchema|type must be JSONType/i,
    "genuinely malformed schemas still fail closed",
  );
});

test("patched advertisement and MCP inventory consume the same registry", () => {
  const { session } = seedSession("registry", "key", [
    { name: "mcp__memory__search", toolName: "mcp__memory__search", description: "search", inputSchema: { type: "object" } },
    { name: "Bash", toolName: "Bash", description: "shell", inputSchema: { type: "object" } },
  ]);
  const servers = buildMcpServers(session);
  const state = headlessMcpState(session).__ccJson.success.servers;
  const requestContext = headlessRequestContext(session).__ccJson.success.requestContext;
  assert.deepEqual(Object.keys(servers).sort(), state.map((server) => server.serverName).sort());
  const inventory = state.flatMap((server) => server.tools.map((tool) => tool.name)).sort();
  assert.deepEqual(inventory, session.advertise.map((tool) => tool.name).sort());
  assert.equal(mcpServerKeyForTool("mcp__memory__search"), "memory");
  assert.equal(mcpServerKeyForTool("Bash"), DEFAULT_MCP_SERVER_KEY);
  assert.equal(mcpServerKeyForTool("_mcp__codebase_memory_mcp_index_status"), DEFAULT_MCP_SERVER_KEY);
  assert.ok(state.flatMap((server) => server.tools).every((tool) => tool.providerIdentifier === CLIENT_TOOL_PROVIDER_ID));
  assert.ok(Object.hasOwn(servers, DEFAULT_MCP_SERVER_KEY));
  assert.equal(servers[DEFAULT_MCP_SERVER_KEY].headers["X-Client-Tools-Session"], session.id);
  assert.deepEqual(sdkAdvertisedTools(session).map((tool) => tool.name).sort(), inventory);
  assert.equal(requestContext.mcpMetaToolOptions.enabled, false);
});

test("the generic MCP server carries tools added after the SDK server map is fixed without duplicates", async () => {
  const session = new Session("dynamic-registry", "key");
  session.advertise = [];
  const initiallyDialed = buildMcpServers(session);
  assert.deepEqual(Object.keys(initiallyDialed), [DEFAULT_MCP_SERVER_KEY]);
  session.mcpServerKeys = Object.freeze(Object.keys(initiallyDialed));
  session.advertise = [
    { name: "mcp__memory__search", inputSchema: { type: "object" } },
    { name: "Bash", inputSchema: { type: "object" } },
  ];
  const state = headlessMcpState(session).__ccJson.success.servers;
  assert.deepEqual(state.map((server) => server.serverName), [DEFAULT_MCP_SERVER_KEY]);
  assert.deepEqual(state[0].tools.map((tool) => tool.name).sort(), ["Bash", "mcp__memory__search"]);
  sessions.set(session.id, session);
  const listed = await mcpDispatch({ jsonrpc: "2.0", id: 32, method: "tools/list" }, session.id, "");
  assert.deepEqual(listed.result.tools.map((tool) => tool.name).sort(), ["Bash", "mcp__memory__search"]);
});

test("new natural MCP servers overflow into generic while existing servers keep one exact copy", () => {
  const session = new Session("dynamic-natural-registry", "key");
  session.advertise = [{ name: "mcp__memory__search", inputSchema: { type: "object" } }];
  session.mcpServerKeys = Object.freeze(Object.keys(buildMcpServers(session)));
  assert.deepEqual([...session.mcpServerKeys].sort(), [DEFAULT_MCP_SERVER_KEY, "memory"].sort());
  session.advertise = [
    { name: "mcp__memory__search", inputSchema: { type: "object" } },
    { name: "mcp__github__get_issue", inputSchema: { type: "object" } },
    { name: "Read", inputSchema: { type: "object" } },
  ];
  const state = headlessMcpState(session).__ccJson.success.servers;
  const byServer = Object.fromEntries(state.map((server) => [server.serverName, server.tools.map((tool) => tool.name).sort()]));
  assert.deepEqual(byServer.memory, ["mcp__memory__search"]);
  assert.deepEqual(byServer[DEFAULT_MCP_SERVER_KEY], ["Read", "mcp__github__get_issue"]);
  assert.equal(state.flatMap((server) => server.tools).length, 3);
});

test("reserved JavaScript property names remain valid MCP server keys", () => {
  const session = new Session("reserved-server-keys", "key");
  session.advertise = [
    { name: "mcp__constructor__lookup", inputSchema: { type: "object" } },
    { name: "mcp__toString__read", inputSchema: { type: "object" } },
  ];
  const servers = buildMcpServers(session);
  assert.ok(Object.hasOwn(servers, "constructor"));
  assert.ok(Object.hasOwn(servers, "toString"));
  const inventory = headlessMcpState(session).__ccJson.success.servers.flatMap((server) => server.tools.map((tool) => tool.name));
  assert.deepEqual(inventory.sort(), ["mcp__constructor__lookup", "mcp__toString__read"].sort());
});

test("MCP server keys cannot become URL dot segments", () => {
  assert.equal(mcpServerKeyForTool(`mcp__${"."}__lookup`), "server-dot");
  assert.equal(mcpServerKeyForTool(`mcp__${".."}__lookup`), "server-dotdot");
});

test("tool manifest names client tools directly without teaching transport-specific wrappers", () => {
  const manifest = toolManifest([
    { name: "_mcp__codebase_memory_mcp_index_status", description: "status", inputSchema: { type: "object" } },
    { name: "Read", description: "read", inputSchema: { type: "object" } },
  ]);
  assert.match(manifest, /Available client tools this turn/);
  assert.match(manifest, /_mcp__codebase_memory_mcp_index_status/);
  assert.match(manifest, /- Read/);
  assert.doesNotMatch(manifest, /claude[- ]code/i);
  assert.doesNotMatch(manifest, /MCP server|MCP transport|transport metadata/i);
  assert.doesNotMatch(manifest, /CallMcpTool/);
  assert.doesNotMatch(manifest, /GetMcpTools/);
});

test("schema-invalid patched MCP calls are rejected before client handoff", async () => {
  const { session, output } = seedSession("invalid-patched-mcp", "key", [{
    name: "mcp__codebase_memory_mcp_index_status",
    description: "status",
    inputSchema: {
      type: "object",
      properties: { repo_path: { type: "string" }, project: { type: "string" } },
      required: ["repo_path", "project"],
      additionalProperties: false,
    },
  }]);
  const response = await session.dispatchMcp({
    toolName: "mcp__codebase_memory_mcp_index_status",
    args: {},
    toolCallId: "invalid-sdk-mcp",
  });
  assert.equal(response.__ccJson.success.isError, true);
  assert.equal(response.__ccJson.success.structuredContent.code, "client_tool_invalid_arguments");
  assert.equal(response.__ccJson.success.structuredContent.executed, false);
  assert.deepEqual(response.__ccJson.success.structuredContent.errors.map((error) => error.path).sort(), ["project", "repo_path"]);
  assert.equal(session.currentRound, null);
  assert.equal(session.invalidToolCalls, 1);
  assert.doesNotMatch(output.text(), /tool_call/);
});

test("schema-invalid HTTP MCP calls use the same preflight and enum validation", async () => {
  const { session, output } = seedSession("invalid-http-mcp", "key", [{
    name: "todo",
    description: "todo",
    inputSchema: {
      type: "object",
      properties: { op: { type: "string", enum: ["init", "view"] } },
      required: ["op"],
      additionalProperties: false,
    },
  }]);
  const response = await mcpDispatch({
    jsonrpc: "2.0",
    id: 9,
    method: "tools/call",
    params: { name: "todo", arguments: { op: "create" } },
  }, session.id, DEFAULT_MCP_SERVER_KEY);
  assert.equal(response.result.isError, true);
  assert.equal(response.result.structuredContent.code, "client_tool_invalid_arguments");
  assert.equal(response.result.structuredContent.executed, false);
  assert.equal(response.result.structuredContent.errors[0].keyword, "enum");
  assert.equal(session.currentRound, null);
  assert.equal(session.invalidToolCalls, 1);
  assert.doesNotMatch(output.text(), /tool_call/);
});

test("repeated schema-invalid calls terminate the SDK run instead of spinning forever", async () => {
  const { session, output } = seedSession("invalid-call-loop", "key", [{
    name: "Required",
    inputSchema: { type: "object", required: ["value"] },
  }]);
  for (let i = 0; i < COMPOSER_MAX_IDENTICAL_INVALID_TOOL_CALLS; i++) {
    const response = await session.dispatchMcp({ toolName: "Required", args: {}, toolCallId: `invalid-${i}` });
    assert.equal(response.__ccJson.success.isError, true);
  }
  assert.equal(session.invalidToolCalls, COMPOSER_MAX_IDENTICAL_INVALID_TOOL_CALLS);
  assert.equal(session.loopTripped, true);
  assert.match(output.text(), /repeated the same internally rejected client-tool call/);
  assert.match(output.text(), /"error_code":"client_tool_contract_mismatch"/);
  assert.match(output.text(), /"executed":false/);
  assert.doesNotMatch(output.text(), /"type":"tool_call"/);
  const auditFile = path.join(TEST_STATE_ROOT, "internal-tool-attempts.jsonl");
  const sessionHash = createHash("sha256").update(session.id).digest("hex");
  const attempts = readFileSync(auditFile, "utf8").trim().split("\n")
    .map((line) => JSON.parse(line))
    .filter((attempt) => attempt.sessionIdHash === sessionHash);
  assert.equal(attempts.length, COMPOSER_MAX_IDENTICAL_INVALID_TOOL_CALLS);
  assert.ok(attempts.every((attempt) => attempt.executed === false && attempt.schemaFingerprint
    && attempt.argumentFingerprint && attempt.errorSignature));
  assert.ok(attempts.every((attempt) => !Object.hasOwn(attempt, "arguments")));
});

test("different corrective attempts with the same validation keyword are not a false identical loop", async () => {
  const { session } = seedSession("distinct-invalid-corrections", "key", [{
    name: "RequiredString",
    inputSchema: {
      type: "object",
      properties: { value: { type: "string" } },
      required: ["value"],
      additionalProperties: false,
    },
  }]);
  for (let i = 0; i < COMPOSER_MAX_IDENTICAL_INVALID_TOOL_CALLS; i++) {
    const response = await session.dispatchMcp({
      toolName: "RequiredString",
      args: { value: i },
      toolCallId: `different-invalid-${i}`,
    });
    assert.equal(response.__ccJson.success.isError, true);
  }
  assert.equal(session.invalidToolCalls, COMPOSER_MAX_IDENTICAL_INVALID_TOOL_CALLS);
  assert.equal(session.loopTripped, false);
  assert.ok(session.internalRejectionSignatures.size >= COMPOSER_MAX_IDENTICAL_INVALID_TOOL_CALLS);
  assert.ok(session.invalidToolCalls < COMPOSER_MAX_INVALID_TOOL_CALLS);
});

test("prototype-named rejected arguments retain distinct privacy-safe loop signatures", async () => {
  const { session } = seedSession("prototype-attempt-signatures", "key", [{
    name: "Strict",
    inputSchema: {
      type: "object",
      properties: { value: { type: "string" } },
      required: ["value"],
      additionalProperties: false,
    },
  }]);
  for (const marker of ["one", "two"]) {
    const args = JSON.parse(`{"value":7,"__proto__":"${marker}"}`);
    const response = await session.dispatchMcp({ toolName: "Strict", args, toolCallId: `prototype-${marker}` });
    assert.equal(response.__ccJson.success.isError, true);
  }
  const sessionHash = createHash("sha256").update(session.id).digest("hex");
  const attempts = readFileSync(path.join(TEST_STATE_ROOT, "internal-tool-attempts.jsonl"), "utf8").trim().split("\n")
    .map((line) => JSON.parse(line))
    .filter((attempt) => attempt.sessionIdHash === sessionHash);
  assert.equal(attempts.length, 2);
  assert.notEqual(attempts[0].argumentFingerprint, attempts[1].argumentFingerprint);
  assert.notEqual(attempts[0].errorSignature, attempts[1].errorSignature);
  assert.equal(session.loopTripped, false);
});

test("HTTP unknown tools and unmatched native retries share the same terminal rejection bound", async () => {
  const http = seedSession("unknown-http-loop", "key", advertised("Lookup"));
  http.session.stepToolStarted = 2;
  for (let i = 0; i < COMPOSER_MAX_IDENTICAL_INVALID_TOOL_CALLS; i++) {
    const response = await mcpDispatch({
      jsonrpc: "2.0",
      id: i,
      method: "tools/call",
      params: { name: "Missing", arguments: {} },
    }, http.session.id, DEFAULT_MCP_SERVER_KEY);
    assert.equal(response.result.isError, true);
    if (i === 0) assert.equal(http.session.stepToolStarted, 1);
  }
  assert.equal(http.session.loopTripped, true);
  assert.equal(http.session.stepToolStarted, 0, "HTTP MCP hidden rejections must settle announced SDK calls");
  assert.equal(http.session.currentRound, null);
  assert.match(http.output.text(), /repeated the same internally rejected client-tool call/);

  const native = seedSession("unknown-native-loop", "key", []);
  for (let i = 0; i < COMPOSER_MAX_IDENTICAL_INVALID_TOOL_CALLS; i++) {
    const response = await native.session.dispatchUnary("readArgs", CC_CASES.readArgs, {
      toolCallId: `missing-native-${i}`,
      path: "/repo/a",
    });
    assert.ok(response.__ccJson.error);
  }
  assert.equal(native.session.loopTripped, true);
  assert.equal(native.session.currentRound, null);
  assert.match(native.output.text(), /repeated the same internally rejected client-tool call/);
});

test("repeated synthetic artifact retries terminate without ever reaching the harness", async () => {
  const artifact = "agent-tools/aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee.txt";
  const { session, output } = seedSession("synthetic-artifact-loop", "key", [{
    name: "write_file",
    inputSchema: { type: "object", properties: { path: { type: "string" }, content: { type: "string" } }, required: ["path"] },
  }]);
  for (let i = 0; i < COMPOSER_MAX_IDENTICAL_INVALID_TOOL_CALLS; i++) {
    const result = await session.openClientTool({
      source: "test",
      rawToolCallId: `artifact-${i}`,
      name: "write_file",
      input: { path: artifact, content: "result copy" },
      resultAdapter: (value) => value,
    });
    assert.equal(result.isError, true);
  }
  assert.equal(session.loopTripped, true);
  assert.equal(session.currentRound, null);
  assert.doesNotMatch(output.text(), /"type":"tool_call"/);
});

test("schema preflight runs after safe wrapper normalization and valid calls still open ToolRound", async () => {
  const { session } = seedSession("normalized-valid-mcp", "key", [{
    name: "Lookup",
    description: "lookup",
    inputSchema: {
      type: "object",
      properties: { q: { type: "string" } },
      required: ["q"],
      additionalProperties: false,
    },
  }]);
  const opened = await openTool(session, { input: { q: { text: "normalized" } } });
  assert.deepEqual(opened.call.input, { q: "normalized" });
  assert.equal(session.invalidToolCalls, 0);
  await session.cancel({ terminalReason: "test_cleanup", detail: "valid normalization test complete" });
  await assert.rejects(opened.promise, /test_cleanup/);
});

test("native read, write, and shell dispatch use the canonical client tool contracts", async () => {
  const cases = [
    {
      id: "native-read-contract",
      cas: "readArgs",
      spec: CC_CASES.readArgs,
      sdk: { path: "/repo/a.py", offset: 3, limit: 4, toolCallId: "native-read" },
      tool: "_read",
      schema: {
        type: "object",
        properties: { path: { type: "string" }, offset: { type: "number" }, limit: { type: "number" } },
        required: ["path"],
      },
      expected: { path: "/repo/a.py", offset: 3, limit: 4 },
    },
    {
      id: "native-write-contract",
      cas: "writeArgs",
      spec: CC_CASES.writeArgs,
      sdk: { path: "/repo/new.py", fileText: "print('ok')", toolCallId: "native-write" },
      tool: "_write",
      schema: {
        type: "object",
        properties: { path: { type: "string" }, content: { type: "string" } },
        required: ["path", "content"],
        additionalProperties: false,
      },
      expected: { path: "/repo/new.py", content: "print('ok')" },
    },
    {
      id: "native-shell-contract",
      cas: "shellArgs",
      spec: CC_CASES.shellArgs,
      sdk: { command: "pwd", workingDirectory: "/repo", toolCallId: "native-shell" },
      tool: "_bash",
      schema: {
        type: "object",
        properties: { command: { type: "string" }, cwd: { type: "string" } },
        required: ["command"],
        additionalProperties: false,
      },
      expected: { command: "pwd", cwd: "/repo" },
    },
  ];

  for (const item of cases) {
    const { session } = seedSession(item.id, "key", [{ name: item.tool, inputSchema: item.schema }]);
    const promise = session.dispatchUnary(item.cas, item.spec, item.sdk);
    promise.catch(() => {});
    await waitFor(() => session.currentRound && session.currentRound.fifo.length === 1, `${item.cas} ToolRound`);
    const call = session.currentRound.calls.get(session.currentRound.fifo[0]);
    assert.equal(call.name, item.tool);
    assert.deepEqual(call.input, item.expected);
    assert.equal(session.invalidToolCalls, 0);
    await session.cancel({ terminalReason: "test_cleanup", detail: `${item.cas} complete` });
    await assert.rejects(promise, /test_cleanup/);
    sessions.delete(session.id);
  }
});

test("native dispatch fails closed when no advertised client capability matches", async () => {
  const { session } = seedSession("native-no-match", "key", [{
    name: "delete_everything",
    inputSchema: { type: "object", properties: { path: { type: "string" } }, required: ["path"] },
  }]);
  const unary = await session.dispatchUnary("readArgs", CC_CASES.readArgs, { path: "/repo/a", toolCallId: "native-read" });
  assert.match(unary.__ccJson.error.error, /no advertised client tool safely matches/);
  assert.equal(session.currentRound, null);

  const chunks = [];
  for await (const chunk of session.dispatchStream("shellStreamArgs", CC_CASES.shellStreamArgs, {
    command: "pwd",
    toolCallId: "native-shell",
  })) chunks.push(chunk.__ccJson);
  assert.equal(chunks.at(-1).exit.code, 1);
  assert.equal(chunks.at(-1).exit.aborted, true);
  assert.equal(session.currentRound, null);
  sessions.delete(session.id);
});

test("native SDK capabilities route bidirectionally to common client spellings", async () => {
  const cases = [
    {
      id: "native-read-file",
      cas: "readArgs",
      spec: CC_CASES.readArgs,
      sdk: { path: "/repo/a", toolCallId: "read-file" },
      tool: "read_file",
      schema: { type: "object", properties: { file_path: { type: "string" } }, required: ["file_path"], additionalProperties: false },
      expected: { file_path: "/repo/a" },
    },
    {
      id: "native-write-file",
      cas: "writeArgs",
      spec: CC_CASES.writeArgs,
      sdk: { path: "/repo/a", fileText: "body", toolCallId: "write-file" },
      tool: "write_file",
      schema: {
        type: "object",
        properties: { file_path: { type: "string" }, file_text: { type: "string" } },
        required: ["file_path", "file_text"],
        additionalProperties: false,
      },
      expected: { file_path: "/repo/a", file_text: "body" },
    },
    {
      id: "native-terminal",
      cas: "shellArgs",
      spec: CC_CASES.shellArgs,
      sdk: { command: "pwd", workingDirectory: "/repo", toolCallId: "terminal" },
      tool: "run_terminal_cmd",
      schema: {
        type: "object",
        properties: { command: { type: "string" }, working_directory: { type: "string" } },
        required: ["command"],
        additionalProperties: false,
      },
      expected: { command: "pwd", working_directory: "/repo" },
    },
    {
      id: "native-remove-file",
      cas: "deleteArgs",
      spec: CC_CASES.deleteArgs,
      sdk: { path: "/repo/a", toolCallId: "remove-file" },
      tool: "remove_file",
      schema: { type: "object", properties: { file_path: { type: "string" } }, required: ["file_path"], additionalProperties: false },
      expected: { file_path: "/repo/a" },
    },
  ];
  for (const item of cases) {
    const { session } = seedSession(item.id, "key", [{ name: item.tool, inputSchema: item.schema }]);
    const pending = session.dispatchUnary(item.cas, item.spec, item.sdk);
    pending.catch(() => {});
    await waitFor(() => session.currentRound?.fifo.length === 1, `${item.tool} handoff`);
    const call = session.currentRound.calls.get(session.currentRound.fifo[0]);
    assert.equal(call.name, item.tool);
    assert.deepEqual(call.input, item.expected);
    await session.cancel({ terminalReason: "test_cleanup", detail: item.tool });
    await assert.rejects(pending, /test_cleanup/);
    sessions.delete(session.id);
  }
});

test("StrReplace and TodoWrite families translate into arbitrary client contracts before handoff", async () => {
  const { session } = seedSession("native-name-families", "key", [
    {
      name: "_edit",
      inputSchema: {
        type: "object",
        properties: {
          path: { type: "string" },
          old_string: { type: "string" },
          new_string: { type: "string" },
        },
        required: ["path", "old_string", "new_string"],
        additionalProperties: false,
      },
    },
    {
      name: "_todo",
      inputSchema: {
        type: "object",
        properties: {
          i: {
            type: "string",
            description: "concise intent",
            "x-cliproxy-client-decoration": true,
          },
          op: { type: "string", enum: ["init"] },
          list: { type: "array", items: { type: "object" } },
        },
        required: ["i", "op", "list"],
        additionalProperties: false,
      },
    },
  ]);

  const editPromise = session.dispatchMcp({
    toolName: "StrReplace",
    toolCallId: "str-replace",
    args: { filePath: "/repo/a.py", oldString: "before", newString: "after" },
  });
  editPromise.catch(() => {});
  await waitFor(() => session.currentRound && session.currentRound.fifo.length === 1, "translated edit call");
  let call = session.currentRound.calls.get(session.currentRound.fifo[0]);
  assert.equal(call.name, "_edit");
  assert.deepEqual(call.input, { path: "/repo/a.py", old_string: "before", new_string: "after" });
  await session.cancel({ terminalReason: "test_cleanup", detail: "edit complete" });
  await assert.rejects(editPromise, /test_cleanup/);

  session.done = false;
  session.beginResponse(new MockResponse());
  const todoPromise = session.dispatchMcp({
    toolName: "TodoWrite",
    toolCallId: "todo-write",
    args: { op: { op: "init" }, list: { list: [{ task: "P0" }] } },
  });
  todoPromise.catch(() => {});
  await waitFor(() => session.currentRound && session.currentRound.fifo.length === 1, "translated todo call");
  call = session.currentRound.calls.get(session.currentRound.fifo[0]);
  assert.equal(call.name, "_todo");
  assert.deepEqual(call.input, { op: "init", list: [{ task: "P0" }] });
  assert.equal(session.invalidToolCalls, 0);
  await session.cancel({ terminalReason: "test_cleanup", detail: "todo complete" });
  await assert.rejects(todoPromise, /test_cleanup/);
});

test("replacing the advertised registry also replaces its schema validator", () => {
  const registry = new AdvertisedToolRegistry();
  registry.replace([{ name: "A", inputSchema: { type: "object", required: ["first"] } }]);
  const firstFailure = registry.validate("A", {});
  assert.equal(firstFailure.structuredContent.errors[0].path, "first");
  registry.replace([{ name: "A", inputSchema: { type: "object", required: ["second"] } }]);
  assert.equal(registry.validate("A", {}).structuredContent.errors[0].path, "second");
});

test("tool-name reconciliation is exact or unambiguous and never arbitrary single-tool routing", () => {
  assert.equal(reconcileExport(advertised("get_weather"), "get-weather"), "get_weather");
  assert.equal(reconcileExport(advertised("Bash"), "nanobanana_generate"), null);
  assert.equal(reconcileExport([{ name: "read_file" }, { name: "write_file" }], "file"), null);
});

test("signed continuation receipts are persisted before the live callback resolves", async () => {
  const { session } = seedSession("continue-live", "cursor-key");
  const events = [];
  const { call, promise, round } = await openTool(session, { adapter: (result) => { events.push("callback"); return result; } });
  const res = new MockResponse();
  const handling = handleContinue(request(), res, continuationBody([
    { toolCallId: call.wireId, content: "done", isError: false, structuredContent: { ok: true } },
  ]), "cursor-key");
  const result = await promise;
  await handling;
  assert.deepEqual(result.structuredContent, { ok: true });
  assert.deepEqual(events, ["callback"]);
  assert.equal(res.status, 200);
  assert.match(res.text(), /duplicate_or_concurrent/);
  assert.match(res.text(), /"contractVersion":2/);
  assert.match(res.text(), /"acknowledgedToolResultIds":\["cct1_/);
  assert.match(res.text(), /"toolEpochState":\{/);
  const saved = journalRecord(round.route);
  assert.ok(saved.calls[0].resultHash);
  assert.deepEqual(saved.calls[0].receipt.result.structuredContent, { ok: true });
  assert.equal(saved.terminal.reason, "completed");
});

test("malformed optional result projections become model-visible errors instead of client 422", async () => {
  const { session } = seedSession("accommodate-result-envelope", "cursor-key");
  const opened = await openTool(session);
  const res = new MockResponse();
  const handling = handleContinue(request(), res, continuationBody([{
    toolCallId: opened.call.wireId,
    content: "the useful textual result",
    isError: "false",
    images: [{ data: "not base64!", mimeType: 7 }],
  }]), "cursor-key");
  const applied = await opened.promise;
  await handling;
  assert.equal(res.status, 200);
  assert.equal(applied.isError, true);
  assert.match(applied.content, /the useful textual result/);
  assert.match(applied.content, /proxy accommodated malformed client-tool result/);
  const saved = journalRecord(opened.round.route).calls[0].receipt.result;
  assert.equal(saved.isError, true);
  assert.deepEqual(saved.images, []);
});

test("invalid replacement inventory is adapted without stranding an already-executed tool result", async () => {
  const { session } = seedSession("invalid-continuation-schema", "cursor-key");
  const opened = await openTool(session);
  let callbackSettled = false;
  void opened.promise.then(() => { callbackSettled = true; }, () => { callbackSettled = true; });
  const res = new MockResponse();
  await handleContinue(request(), res, continuationBody([
    { toolCallId: opened.call.wireId, content: "must-commit" },
  ], {}, {
    tools: [{ name: "Broken", inputSchema: { type: "not-a-json-schema-type" } }],
  }), "cursor-key");
  assert.equal(res.status, 200);
  assert.doesNotMatch(res.text(), /"toolInventory":\{"status":"rejected"/);
  assert.equal((await opened.promise).content, "must-commit");
  assert.equal(callbackSettled, true);
  assert.ok(journalRecord(opened.round.route).calls[0].resultHash);
  assert.deepEqual(session.advertise.map((tool) => tool.name), ["Lookup"]);
});

test("whole-batch token validation is atomic: one tampered id commits nothing", async () => {
  const { session } = seedSession("atomic", "cursor-key");
  const first = await openTool(session, { rawId: "one", awaiting: false });
  // Open a second call directly in the same collecting round before awaiting state.
  const secondPromise = session.openClientTool({ source: "test", rawToolCallId: "two", name: "Lookup", input: { q: "two" }, resultAdapter: (x) => x });
  secondPromise.catch(() => {});
  await waitFor(() => first.round.fifo.length === 2 && first.round.calls.get(first.round.fifo[1]).handedAt != null, "second handoff");
  if (session.flushTimer) { clearTimeout(session.flushTimer); session.flushTimer = null; }
  const second = first.round.calls.get(first.round.fifo[1]);
  first.round.markAwaitingResults();
  const tampered = second.wireId.slice(0, -1) + (second.wireId.endsWith("A") ? "B" : "A");
  const res = new MockResponse();
  await handleContinue(request(), res, continuationBody([
    { toolCallId: first.call.wireId, content: "valid" },
    { toolCallId: tampered, content: "invalid" },
  ]), "cursor-key");
  assert.equal(res.status, 400);
  assert.equal(first.round.calls.get(first.call.wireId).resultHash, null);
  assert.equal(first.round.calls.get(second.wireId).resultHash, null);
  first.round.terminalize("client_cancelled", "test cleanup");
  await assert.rejects(first.promise);
  await assert.rejects(secondPromise);
});

test("journal CAS races are reconciled without 409 or a fake whole-turn success", async () => {
  const { session } = seedSession("cas-reconcile", "cursor-key");
  const opened = await openTool(session);
  const other = createRoundInfrastructure(TEST_STATE_ROOT);
  const landedElsewhere = ToolRound.load(other.journal, other.codec, opened.round.route);
  const result = { toolCallId: opened.call.wireId, content: "accepted elsewhere", isError: false };
  landedElsewhere.commitResults([result], { allowRegisteredReceipt: true });
  landedElsewhere.terminalize(TerminalReason.COMPLETED, "accepted by the lease owner");

  const res = new MockResponse();
  await handleContinue(request(), res, continuationBody([result]), "cursor-key");
  assert.equal(res.status, 410);
  assert.doesNotMatch(res.text(), /already_applied|\"stop_reason\":\"end_turn\"/);
  await assert.rejects(opened.promise, /ownership advanced/);
});

test("a residual state-machine conflict is contained as typed HTTP-200 SSE", async () => {
  const { session } = seedSession("contained-conflict", "cursor-key");
  const opened = await openTool(session);
  opened.round.commitResults = () => {
    throw new ToolRoundError("synthetic_state_race", "synthetic conflicting transition", 409);
  };
  const res = new MockResponse();
  await handleContinue(request(), res, continuationBody([
    { toolCallId: opened.call.wireId, content: "retryable" },
  ]), "cursor-key");
  assert.equal(res.status, 200);
  assert.match(res.text(), /continuation_conflict_contained/);
  assert.match(res.text(), /synthetic_state_race/);
  assert.equal(journalRecord(opened.round.route).calls[0].receipt, null);
  opened.round.terminalize("client_cancelled", "cleanup");
  await assert.rejects(opened.promise);
});

test("mixed continuation is a complete no-op when the atomic tool-result batch fails", async () => {
  const { session } = seedSession("atomic-mixed", "cursor-key");
  const opened = await openTool(session);
  const unknownSameRoute = opened.round.nextWireId(99);
  const file = path.join(TEST_STATE_ROOT, "client-tool-rounds", `${opened.round.route}.json`);
  const before = readFileSync(file, "utf8");
  for (let retry = 0; retry < 100; retry++) {
    const res = new MockResponse();
    await handleContinue(request(), res, continuationBody([
      { toolCallId: opened.call.wireId, content: "valid but must not commit" },
      { toolCallId: unknownSameRoute, content: "unknown" },
    ], {
      userText: "also continue",
      clientMessageId: "ccm2_atomic_mixed",
      interruptRequested: true,
    }), "cursor-key");
    assert.equal(res.status, 410);
  }
  assert.equal(readFileSync(file, "utf8"), before,
    "one hundred rejected retries must not change the journal revision or file bytes");
  const saved = journalRecord(opened.round.route);
  assert.equal(saved.calls[0].resultHash, null);
  assert.equal(saved.deferredInputs.length, 0,
    "a rejected HTTP request must not mutate the journal or poison its later retry");

  opened.round.terminalize("client_cancelled", "cleanup");
  await assert.rejects(opened.promise);
});

test("same tool id twice with conflicting projections retains the first result and returns 200", async () => {
  const { session } = seedSession("same-batch-first-wins", "cursor-key");
  const opened = await openTool(session);
  const res = new MockResponse();
  const handling = handleContinue(request(), res, continuationBody([
    { toolCallId: opened.call.wireId, content: "first durable projection", isError: false },
    { toolCallId: opened.call.wireId, content: "later contradictory projection", isError: true },
  ]), "cursor-key");
  assert.equal((await opened.promise).content, "first durable projection");
  await handling;
  assert.equal(res.status, 200);
  assert.match(res.text(), /first_batch_result_retained/);
  assert.match(res.text(), /ignoredConflictingDuplicate/);
  const saved = journalRecord(opened.round.route).calls[0];
  assert.equal(saved.receipt.result.content, "first durable projection");
  assert.equal(saved.receipt.result.isError, false);
});

test("two genuinely live signed routes return a typed non-mutating receipt instead of HTTP 409", async () => {
  const a = seedSession("route-a", "cursor-key").session;
  const openA = await openTool(a, { rawId: "a" });
  const b = seedSession("route-b", "cursor-key").session;
  const openB = await openTool(b, { rawId: "b" });
  const res = new MockResponse();
  await handleContinue(request(), res, continuationBody([
    { toolCallId: openA.call.wireId, content: "A" },
    { toolCallId: openB.call.wireId, content: "B" },
  ]), "cursor-key");
  assert.equal(res.status, 200);
  assert.match(res.text(), /multiple_live_tool_rounds_deferred/);
  assert.match(res.text(), /no result was consumed/);
  assert.equal(openA.call.resultHash, null);
  assert.equal(openB.call.resultHash, null);
  openA.round.terminalize("client_cancelled", "cleanup");
  openB.round.terminalize("client_cancelled", "cleanup");
  await assert.rejects(openA.promise);
  await assert.rejects(openB.promise);
});

test("mixed-route continuation acknowledges terminal history and applies the one live round", async () => {
  const oldSession = seedSession("mixed-route-old", "cursor-key").session;
  const oldCall = await openTool(oldSession, { rawId: "old" });
  const oldResult = { toolCallId: oldCall.call.wireId, content: "old durable value", isError: false };
  oldCall.round.applyResults([oldResult]);
  await oldCall.promise;

  const liveSession = seedSession("mixed-route-live", "cursor-key").session;
  const liveCall = await openTool(liveSession, { rawId: "live" });
  const liveResult = { toolCallId: liveCall.call.wireId, content: "live value", isError: false };
  const res = new MockResponse();
  const handling = handleContinue(request(), res, continuationBody([
    { ...oldResult, content: "historical projection drift" },
    liveResult,
  ]), "cursor-key");
  assert.equal((await liveCall.promise).content, "live value");
  await handling;
  assert.equal(res.status, 200);
  assert.match(res.text(), /historical_receipt_acknowledged/);
  assert.match(res.text(), new RegExp(oldCall.call.wireId));
  assert.equal(journalRecord(oldCall.round.route).calls[0].receipt.result.content, "old durable value");
});

test("an expired historical route cannot make a valid mixed-route continuation fail wholesale", async () => {
  const { codec } = createRoundInfrastructure(TEST_STATE_ROOT);
  const absentRoute = codec.newRoute();
  const absentId = codec.issue(absentRoute, 0);
  const liveSession = seedSession("mixed-route-with-expired-history", "cursor-key").session;
  const liveCall = await openTool(liveSession, { rawId: "live" });
  const res = new MockResponse();
  const handling = handleContinue(request(), res, continuationBody([
    { toolCallId: absentId, content: "expired historical result" },
    { toolCallId: liveCall.call.wireId, content: "current live result" },
  ]), "cursor-key");
  assert.equal((await liveCall.promise).content, "current live result");
  await handling;
  assert.equal(res.status, 200);
  assert.match(res.text(), /historical_route_absent_not_applied/);
  assert.match(res.text(), new RegExp(absentId));
});

test("signed round tenant fingerprint prevents cross-credential result injection", async () => {
  const { session } = seedSession("tenant", "key-a");
  const opened = await openTool(session);
  const res = new MockResponse();
  await handleContinue(request(), res, continuationBody([{ toolCallId: opened.call.wireId, content: "stolen" }]), "key-b");
  assert.equal(res.status, 403);
  assert.equal(opened.call.resultHash, null);
  opened.round.terminalize("client_cancelled", "cleanup");
  await assert.rejects(opened.promise);
});

test("multi-tenant continuation refuses a round with a missing tenant fingerprint", () => {
  assert.equal(continuationTenantMismatch({ tenantFingerprint: "" }, "cursor-key", true), true);
  assert.equal(continuationTenantMismatch({}, "cursor-key", true), true);
  assert.equal(continuationTenantMismatch({ tenantFingerprint: "" }, "cursor-key", false), false);
  assert.equal(continuationTenantMismatch({ tenantFingerprint: keyFingerprint("cursor-key") }, "cursor-key", true), false);
  assert.equal(continuationTenantMismatch({ tenantFingerprint: keyFingerprint("other-key") }, "cursor-key", true), true);
});

test("a valid signed result is receipted across the handoff timestamp crash window", async () => {
  const session = new Session("not-handed", "key");
  session.clientEnv = { workspaceUnknown: true };
  const round = session.ensureToolRound();
  const call = round.openCall({ source: "test", rawToolCallId: "raw", name: "Lookup", input: {}, callback: { resolve() {}, reject() {} } });
  round.markAwaitingResults();
  const res = new MockResponse();
  await handleContinue(request(), res, continuationBody([{ toolCallId: call.wireId, content: "early" }]), "key");
  assert.equal(res.status, 410, "missing recovery history is separate from accepting the signed result");
  assert.equal(round.calls.get(call.wireId).receipt.result.content, "early");
});

test("restart without bounded history durably receipts the result then refuses faithful recovery", async () => {
  const { session } = seedSession("restart-refuse", "cursor-key");
  const opened = await openTool(session);
  opened.round.clearTimer(opened.call);
  liveToolRounds.delete(opened.round.route);
  sessions.delete(session.id);
  const res = new MockResponse();
  await handleContinue(request(), res, continuationBody([{ toolCallId: opened.call.wireId, content: "executed" }]), "cursor-key");
  assert.equal(res.status, 410);
  assert.equal(res.json().error.code, "round_lost");
  const saved = journalRecord(opened.round.route);
  assert.equal(saved.calls[0].receipt.result.content, "executed");
  assert.equal(saved.terminal.reason, "restart_lost");
  assert.equal(saved.recovery.decision, "refused");
});

test("restart accepts a registered call receipt when the handoff journal write was the crash boundary", async () => {
  const session = new Session("restart-handoff-window", "cursor-key");
  session.clientEnv = { workspaceUnknown: true };
  const round = session.ensureToolRound();
  const call = round.openCall({ source: "test", rawToolCallId: "registered", name: "Lookup", input: { q: "x" } });
  assert.equal(call.handedAt, null);
  liveToolRounds.delete(round.route);
  sessions.delete(session.id);

  const res = new MockResponse();
  await handleContinue(request(), res, continuationBody([{ toolCallId: call.wireId, content: "executed after frame delivery" }]), "cursor-key");
  assert.equal(res.status, 410);
  const saved = journalRecord(round.route);
  assert.equal(saved.calls[0].receipt.result.content, "executed after frame delivery");
  assert.equal(saved.terminal.reason, "restart_lost");
});

test("late result after a run-error terminal is receipted but never acknowledged as success", async () => {
  const { session } = seedSession("late-after-error", "cursor-key");
  const opened = await openTool(session);
  opened.round.terminalize("run_error", "upstream failed");
  sessions.delete(session.id);
  liveToolRounds.delete(opened.round.route);
  await assert.rejects(opened.promise, /run_error/);

  const res = new MockResponse();
  await handleContinue(request(), res, continuationBody([{ toolCallId: opened.call.wireId, content: "late local result" }]), "cursor-key");
  assert.equal(res.status, 410);
  assert.doesNotMatch(res.text(), /already_applied|end_turn/);
  const saved = journalRecord(opened.round.route);
  assert.equal(saved.calls[0].receipt.result.content, "late local result");
  assert.equal(saved.terminal.reason, "run_error");
});

test("result-adapter failure leaves the durable receipt and terminalizes the round", async () => {
  const { session } = seedSession("adapter-failure", "cursor-key");
  const opened = await openTool(session, { adapter: () => { throw new Error("adapter exploded"); } });
  const res = new MockResponse();
  await handleContinue(request(), res, continuationBody([{ toolCallId: opened.call.wireId, content: "durable first" }]), "cursor-key");
  await assert.rejects(opened.promise, /adapter exploded/);
  assert.equal(res.status, 500);
  const saved = journalRecord(opened.round.route);
  assert.equal(saved.calls[0].receipt.result.content, "durable first");
  assert.equal(saved.terminal.reason, "run_error");
});

test("restart recovery preserves ids, errors, structured data, images, and trailing text", async () => {
  const { session } = seedSession("recovery-shape", "key");
  const opened = await openTool(session);
  const input = {
    type: "tool_results",
    history: "bounded prior conversation",
    userText: "then continue",
    results: [{
      toolCallId: opened.call.wireId,
      content: { message: "failed" },
      isError: true,
      structuredContent: { code: "E_FAIL" },
      images: [{ data: "QUJD", mimeType: "image/png" }, { url: "https://example.test/x.png" }],
    }],
  };
  opened.round.commitResults(input.results);
  const recovery = buildRestartRecoveryInput(opened.round, input);
  assert.equal(recovery.type, "user");
  assert.match(recovery.recoveryContext, new RegExp(opened.call.wireId));
  assert.match(recovery.recoveryContext, /E_FAIL/);
  assert.equal(recovery.text, "then continue");
  assert.doesNotMatch(recovery.recoveryContext, /Continue the original task/);
  assert.match(recovery.recoveryContext, /explicit corrections replace conflicting prior work/);
  assert.doesNotMatch(recovery.recoveryContext, /then continue/,
    "the exact active user instruction must not be folded into reference context");
  assert.equal(recovery.images.length, 2);
  opened.round.terminalize("client_cancelled", "cleanup");
  await assert.rejects(opened.promise);
});

test("restart recovery is rendered only from durable receipts, never contradictory retry projections", async () => {
  const { session } = seedSession("recovery-receipt-authority", "key");
  const opened = await openTool(session);
  const original = {
    toolCallId: opened.call.wireId,
    content: "ignored original projection",
    contentBlocks: [{ type: "text", text: "durable authoritative text" }],
    images: [{ data: "QUJD", mimeType: "image/png" }],
  };
  opened.round.commitResults([original]);

  const retry = {
    type: "tool_results",
    history: "bounded prior conversation",
    results: [{
      ...original,
      content: "contradictory unreceipted retry text",
      images: [{ data: "RElGRkVSRU5U", mimeType: "image/png" }],
    }],
  };
  const recovery = buildRestartRecoveryInput(opened.round, retry);
  assert.match(recovery.recoveryContext, /durable authoritative text/);
  assert.doesNotMatch(recovery.recoveryContext, /contradictory unreceipted retry text/);
  assert.equal(recovery.images.length, 0, "ignored compatibility images must not bypass authoritative contentBlocks");

  opened.round.terminalize("client_cancelled", "cleanup");
  await assert.rejects(opened.promise);
});

test("terminal callback receipt is authoritative but never faked as whole-turn success", async () => {
  const { session } = seedSession("late", "cursor-key");
  const opened = await openTool(session);
  const result = { toolCallId: opened.call.wireId, content: "once", isError: false };
  const first = new MockResponse();
  await Promise.all([handleContinue(request(), first, continuationBody([result]), "cursor-key"), opened.promise]);
  sessions.delete(session.id);
  liveToolRounds.delete(opened.round.route);

  const duplicate = new MockResponse();
  await handleContinue(request(), duplicate, continuationBody([result]), "cursor-key");
  assert.equal(duplicate.status, 410);
  assert.doesNotMatch(duplicate.text(), /already_applied|\"stop_reason\":\"end_turn\"/);
  assert.equal(journalRecord(opened.round.route).calls[0].receipt.result.content, "once");

  const conflict = new MockResponse();
  await handleContinue(request(), conflict, continuationBody([{ ...result, content: "different" }]), "cursor-key");
  assert.equal(conflict.status, 410);
  assert.doesNotMatch(conflict.text(), /already_applied|\"stop_reason\":\"end_turn\"/);
  assert.equal(journalRecord(opened.round.route).calls[0].receipt.result.content, "once");
});

test("durable receipt remains authoritative while sibling tool callbacks are still live", async () => {
  const { session } = seedSession("parallel-receipt-authority", "cursor-key");
  const first = await openTool(session, { rawId: "first", awaiting: false });
  const secondPromise = session.openClientTool({
    source: "test",
    rawToolCallId: "second",
    name: "Lookup",
    input: { q: "second" },
    resultAdapter: (value) => value,
  });
  secondPromise.catch(() => {});
  await waitFor(() => first.round.fifo.length === 2, "parallel call registration");
  session.flushJournaledCalls();
  const second = first.round.calls.get(first.round.fifo[1]);
  await waitFor(() => second.handedAt != null, "second call handoff");
  first.round.markAwaitingResults();

  const durable = { toolCallId: first.call.wireId, content: "first durable", isError: false };
  const partial = new MockResponse();
  const partialHandling = handleContinue(request(), partial, continuationBody([durable]), "cursor-key");
  assert.equal((await first.promise).content, "first durable");
  await partialHandling;

  const final = new MockResponse();
  const finalHandling = handleContinue(request(), final, continuationBody([
    { ...durable, content: "first projection drift" },
    { toolCallId: second.wireId, content: "second durable", isError: false },
  ]), "cursor-key");
  assert.equal((await secondPromise).content, "second durable");
  await finalHandling;
  assert.equal(final.status, 200);
  assert.match(final.text(), /durable_receipt_retained/);
  assert.equal(journalRecord(first.round.route).calls[0].receipt.result.content, "first durable");
});

test("mixed continuation retains terminal receipt and delivers new user intent exactly once", async () => {
  const { session } = seedSession("mixed-durable-input", "cursor-key");
  const opened = await openTool(session);
  const accepted = { toolCallId: opened.call.wireId, content: "accepted result", isError: false };
  opened.round.applyResults([accepted]);
  await opened.promise;
  assert.equal(opened.round.terminal.reason, "completed");

  const mixedInput = {
    userText: "now continue with the next task",
    clientMessageId: "ccm1_mixed_once",
    interruptRequested: true,
    history: "bounded prior conversation",
  };
  const sent = [];
  session.seeded = true;
  session.agent = {
    async send(message) {
      sent.push(message);
      return {
        async wait() { return { status: "finished", result: "continued" }; },
        async cancel() {},
      };
    },
    async close() {},
  };
  const corrected = new MockResponse();
  await handleContinue(request(), corrected, continuationBody([
    { ...accepted, content: "conflicting replay projection" },
  ], mixedInput), "cursor-key");
  await waitFor(() => journalRecord(opened.round.route).deferredInputs[0].state === "DELIVERED", "deferred input delivery receipt");
  await waitFor(() => corrected.ended, "corrected mixed continuation terminal");
  assert.equal(corrected.status, 200);
  assert.equal(sent.length, 1);
  assert.ok(sent[0].endsWith("now continue with the next task"));
  assert.match(sent[0], /CURRENT USER MESSAGE/);
  assert.doesNotMatch(sent[0], /accepted result/);
  assert.equal(journalRecord(opened.round.route).calls[0].receipt.result.content, "accepted result");
  assert.match(corrected.text(), /durable_receipt_retained/);

  const duplicate = new MockResponse();
  await handleContinue(request(), duplicate, continuationBody([accepted], mixedInput), "cursor-key");
  assert.equal(duplicate.status, 200);
  assert.match(duplicate.text(), /completed_turn_replay/);
  assert.match(duplicate.text(), /continued/);
  assert.equal(sent.length, 1);

  const changedResultAfterCompletion = new MockResponse();
  await handleContinue(request(), changedResultAfterCompletion, continuationBody([{
    ...accepted,
    content: "changed after the completed answer",
    isError: true,
    structuredContent: { changed: true },
  }], mixedInput), "cursor-key");
  assert.equal(changedResultAfterCompletion.status, 200);
  assert.match(changedResultAfterCompletion.text(), /"errorCode":"result_conflict"/);
  assert.match(changedResultAfterCompletion.text(), /continuation_conflict_contained/);
  assert.doesNotMatch(changedResultAfterCompletion.text(), /completed_turn_replay|continued/);
  assert.equal(sent.length, 1, "a conflicting retry must not replay or start another model run");

  const completedEntry = [...completedTurnReceipts.entries()]
    .find(([, record]) => record.clientMessageId === mixedInput.clientMessageId && record.status === "completed");
  assert.ok(completedEntry, "the final answer must have a durable replay receipt");
  rmSync(completedEntry[0], { force: true });
  completedTurnReceipts.delete(completedEntry[0]);
  const missingReceipt = new MockResponse();
  await handleContinue(request(), missingReceipt, continuationBody([accepted], mixedInput), "cursor-key");
  assert.equal(missingReceipt.status, 200);
  assert.match(missingReceipt.text(), /completed_response_receipt_unavailable/);
  assert.match(missingReceipt.text(), /"stop_reason":"error"/);
  assert.doesNotMatch(missingReceipt.text(), /user_input_already_delivered/);
  assert.equal(sent.length, 1, "missing answer evidence must not repeat already-finalized side effects");

  const reusedId = new MockResponse();
  await handleContinue(request(), reusedId, continuationBody([accepted], {
    ...mixedInput,
    userText: "a genuinely different next task despite the reused client id",
  }), "cursor-key");
  await waitFor(() => journalRecord(opened.round.route).deferredInputs.at(-1).state === "DELIVERED", "rekeyed input delivery receipt");
  await waitFor(() => reusedId.ended, "rekeyed input terminal");
  assert.equal(reusedId.status, 200);
  assert.match(reusedId.text(), /rekeyed_queued|rekeyed_duplicate/);
  assert.equal(sent.length, 2, "different intent must be delivered once instead of rejected as a 409 conflict");
  const deferred = journalRecord(opened.round.route).deferredInputs;
  assert.notEqual(deferred[0].clientMessageId, deferred.at(-1).clientMessageId);
  assert.equal(deferred.at(-1).requestedClientMessageId, mixedInput.clientMessageId);
});

test("ambiguous agent.send failure retries the exact message with one stable SDK idempotency key", async () => {
  const { session } = seedSession("mixed-send-uncertain", "cursor-key");
  const opened = await openTool(session);
  const accepted = { toolCallId: opened.call.wireId, content: "accepted", isError: false };
  opened.round.applyResults([accepted]);
  await opened.promise;

  let sends = 0;
  const sendMessages = [];
  const sendKeys = [];
  const sendAdvertised = [];
  session.seeded = true;
  session.agent = {
    async send(message, options) {
      sends++;
      sendMessages.push(message);
      sendKeys.push(options.idempotencyKey);
      sendAdvertised.push(session.advertise.map((tool) => tool.name));
      if (sends === 1) throw new Error("connection closed after request write");
      return {
        async wait() { return { status: "finished", result: "recovered exactly once" }; },
        async cancel() {},
      };
    },
    async close() {},
  };
  const mixedInput = {
    userText: "perform this once",
    clientMessageId: "ccm1_uncertain_once",
    interruptRequested: true,
    history: "bounded prior conversation",
    system: "original system",
  };
  const first = new MockResponse();
  await handleContinue(request(), first, continuationBody([accepted], mixedInput), "cursor-key");
  await waitFor(() => first.ended, "ambiguous send error terminal");
  assert.equal(journalRecord(opened.round.route).deferredInputs[0].state, "DELIVERING");
  assert.equal(sends, 1);

  const retry = new MockResponse();
  await handleContinue(request(), retry, continuationBody([accepted], {
    ...mixedInput,
    history: "a changed retry history that must not alter the SDK envelope",
    system: "changed retry system",
  }, authoritativeToolsBody(advertised("NewlyRegisteredTool"), { toolChoice: "auto" })), "cursor-key");
  await waitFor(() => retry.ended, "idempotent uncertain-send retry terminal");
  assert.equal(retry.status, 200);
  assert.match(retry.text(), /recovered exactly once/);
  assert.match(retry.text(), /"stop_reason":"end_turn"/);
  assert.equal(journalRecord(opened.round.route).deferredInputs[0].state, "DELIVERED");
  assert.equal(sends, 2, "the bridge may retry the transport call; the SDK key makes both calls one logical send");
  assert.equal(sendKeys[0], sendKeys[1]);
  assert.equal(sendMessages[0], sendMessages[1]);
  assert.deepEqual(sendAdvertised, [["Lookup"], ["Lookup"]],
    "an uncertain retry must use the persisted tool snapshot even when the client registers a new tool");
  assert.deepEqual(session.advertise.map((tool) => tool.name), ["NewlyRegisteredTool"],
    "the newer client inventory must remain available for the following turn");
  assert.match(sendKeys[0], /^ccsend2_[A-Za-z0-9_-]{24}_[a-f0-9]{24}_g1_/);

  const laterMessages = [];
  session.agent = {
    async send(message) {
      laterMessages.push(message);
      return {
        async wait() { return { status: "finished", result: "recovered" }; },
        async cancel() {},
      };
    },
    async close() {},
  };
  const later = new MockResponse();
  await handleContinue(request(), later, continuationBody([accepted], {
    ...mixedInput,
    userText: "a later independent message",
    clientMessageId: "ccm1_after_uncertain",
  }), "cursor-key");
  await waitFor(() => later.ended, "later message terminal");
  assert.equal(later.status, 200);
  assert.equal(laterMessages.length, 1);
  assert.ok(laterMessages[0].endsWith("a later independent message"));
});

test("an old identical checkpoint message never suppresses the current exact DELIVERING retry", async () => {
  const cursorKey = "checkpoint-key";
  const { session } = seedSession("checkpoint-delivery", cursorKey);
  const opened = await openTool(session);
  const accepted = { toolCallId: opened.call.wireId, content: "accepted", isError: false };
  opened.round.applyResults([accepted]);
  await opened.promise;

  const input = {
    userText: "message already accepted before the bridge crashed",
    clientMessageId: "ccm1_checkpoint_delivery",
    interruptRequested: true,
    history: "bounded history",
  };
  opened.round.queueDeferredInput(input.clientMessageId, input);
  opened.round.markDeferredInputState(input.clientMessageId, "DELIVERING", {
    agentId: session.agentId,
    textHash: createHash("sha256").update(input.userText).digest("hex"),
    hasImages: false,
    idempotencyKey: "ccsend2_checkpoint_collision_guard",
    message: input.userText,
    advertise: session.advertise,
    model: "cursor-grok-4.5",
  });
  sessions.delete(session.id);
  liveToolRounds.delete(opened.round.route);
  const sends = [];
  const agent = {
    async send(message, options) {
      sends.push({ message, options });
      return {
        async wait() { return { status: "finished", result: "current message delivered" }; },
        async cancel() {},
      };
    },
    async close() {},
  };
  platforms.set(keyHash(cursorKey), {
    promise: Promise.resolve({
      async resumeAgent(agentId) {
        assert.equal(agentId, session.agentId);
        return agent;
      },
      async getAgentMessages() {
        return [{
          type: "user",
          message: {
            turn: {
              case: "agentConversationTurn",
              value: { userMessage: { text: input.userText }, steps: [] },
            },
          },
        }];
      },
    }),
    stateRoot: TEST_STATE_ROOT,
    lastUsed: Date.now(),
    fp: keyFingerprint(cursorKey),
  });

  const retry = new MockResponse();
  await handleContinue(request(), retry, continuationBody([accepted], input), cursorKey);
  await waitFor(() => retry.ended, "checkpoint collision retry terminal");
  assert.equal(retry.status, 200);
  assert.match(retry.text(), /current message delivered/);
  assert.equal(sends.length, 1);
  assert.equal(sends[0].options.idempotencyKey, "ccsend2_checkpoint_collision_guard");
  const saved = journalRecord(opened.round.route).deferredInputs.find((entry) => entry.clientMessageId === input.clientMessageId);
  assert.equal(saved.state, "DELIVERED");
  assert.notEqual(saved.deliveryEvidence, "durable_checkpoint");
});

test("cold retry resends unresolved DELIVERING with the exact persisted SDK idempotency key", async () => {
  const cursorKey = "cold-idempotent-key";
  const { session } = seedSession("cold-idempotent-delivery", cursorKey);
  const opened = await openTool(session);
  const accepted = { toolCallId: opened.call.wireId, content: "accepted", isError: false };
  opened.round.applyResults([accepted]);
  await opened.promise;

  const input = {
    userText: "deliver this exact stored input",
    clientMessageId: "ccm2_cold_delivery_retry",
    interruptRequested: true,
    history: "bounded history",
  };
  const persistedKey = "ccsend2_persisted_crash_boundary_key";
  opened.round.queueDeferredInput(input.clientMessageId, input);
  opened.round.markDeferredInputState(input.clientMessageId, "DELIVERING", {
    agentId: session.agentId,
    textHash: createHash("sha256").update(input.userText).digest("hex"),
    hasImages: false,
    idempotencyKey: persistedKey,
    message: input.userText,
    advertise: session.advertise,
    model: "cursor-grok-4.5",
  });
  sessions.delete(session.id);
  liveToolRounds.delete(opened.round.route);

  const sends = [];
  const agent = {
    async send(message, options) {
      sends.push({ message, options });
      return {
        async wait() { return { status: "finished", result: "cold retry completed" }; },
        async cancel() {},
      };
    },
    async close() {},
  };
  platforms.set(keyHash(cursorKey), {
    promise: Promise.resolve({
      async resumeAgent(agentId) {
        assert.equal(agentId, session.agentId, "uncertain retry must resume the original durable agent");
        return agent;
      },
      async createAgent() { return agent; },
      async getAgentMessages() { return []; },
    }),
    stateRoot: TEST_STATE_ROOT,
    lastUsed: Date.now(),
    fp: keyFingerprint(cursorKey),
  });

  const retry = new MockResponse();
  await handleContinue(request(), retry, continuationBody([accepted], input), cursorKey);
  await waitFor(() => retry.ended, "cold idempotent delivery retry terminal");
  assert.equal(retry.status, 200);
  assert.match(retry.text(), /cold retry completed/);
  assert.equal(sends.length, 1);
  assert.equal(sends[0].options.idempotencyKey, persistedKey);
  assert.match(String(sends[0].message), /deliver this exact stored input/);
  const saved = journalRecord(opened.round.route).deferredInputs
    .find((entry) => entry.clientMessageId === input.clientMessageId);
  assert.equal(saved.state, "DELIVERED");
  assert.equal(saved.deliveryIdempotencyKey, persistedKey);
});

test("same-message retry hands off to the active SDK turn instead of sending twice", async () => {
  const { session } = seedSession("active-delivery-handoff", "cursor-key");
  const opened = await openTool(session);
  const accepted = { toolCallId: opened.call.wireId, content: "accepted", isError: false };
  opened.round.applyResults([accepted]);
  await opened.promise;
  session.seeded = true;

  let releaseSend;
  const sendGate = new Promise((resolve) => { releaseSend = resolve; });
  let sends = 0;
  session.agent = {
    async send() {
      sends++;
      await sendGate;
      return {
        async wait() { return { status: "finished", result: "one answer" }; },
        async cancel() {},
      };
    },
    async close() {},
  };
  const input = {
    userText: "perform one operation",
    clientMessageId: "ccm1_active_handoff",
    interruptRequested: true,
    history: "bounded history",
  };
  const first = new MockResponse();
  const firstRun = handleContinue(request(), first, continuationBody([accepted], input), "cursor-key");
  await waitFor(() => {
    const item = journalRecord(opened.round.route).deferredInputs.find((entry) => entry.clientMessageId === input.clientMessageId);
    return item && item.state === "DELIVERING" && session.activeDeferredInputId === input.clientMessageId;
  }, "active deferred delivery");

  const retry = new MockResponse();
  const retryRun = handleContinue(request(), retry, continuationBody([accepted], input), "cursor-key");
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(sends, 1);
  releaseSend();
  await Promise.all([firstRun, retryRun]);
  assert.equal(retry.status, 200);
  assert.match(retry.text(), /active_turn_handoff/);
  assert.match(retry.text(), /one answer/);
  assert.equal(sends, 1);
});

test("duplicate fresh POST attaches to the active SDK send instead of interrupting itself", async () => {
  const session = new Session("fresh-active-handoff", "cursor-key");
  session.clientEnv = { workspaceUnknown: true };
  session.seeded = true;
  sessions.set(session.id, session);
  let releaseSend;
  const sendGate = new Promise((resolve) => { releaseSend = resolve; });
  let sends = 0;
  let observedIdempotencyKey = "";
  session.agent = {
    async send(_message, options) {
      sends++;
      observedIdempotencyKey = options.idempotencyKey;
      await sendGate;
      return {
        async wait() { return { status: "finished", result: "fresh answer" }; },
        async cancel() {},
      };
    },
    async close() {},
  };
  const body = neutralBody({
    sessionId: session.id,
    model: "cursor-grok-4.5",
    input: { type: "user", text: "one fresh request", clientMessageId: "ccm2_fresh_active" },
  });
  const first = new MockResponse();
  const firstRun = handleTurn(request(), first, body, "cursor-key");
  await waitFor(() => sends === 1 && session.sendPending, "fresh send pending");
  const retry = new MockResponse();
  const retryRun = handleTurn(request(), retry, body, "cursor-key");
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(sends, 1);
  releaseSend();
  await Promise.all([firstRun, retryRun]);
  assert.match(observedIdempotencyKey, /^ccsend2_[A-Za-z0-9_-]{24}_[a-f0-9]{24}_g1_ccm2_fresh_active$/);
  assert.match(retry.text(), /active_turn_handoff/);
  assert.match(retry.text(), /fresh answer/);
  assert.equal(sends, 1);
});

test("active retry replays ordered reasoning, text, and tool frames before live continuation", async () => {
  const session = new Session("fresh-active-frame-replay", "cursor-key");
  session.clientEnv = { workspaceUnknown: true };
  session.seeded = true;
  sessions.set(session.id, session);
  let releaseSend;
  const sendGate = new Promise((resolve) => { releaseSend = resolve; });
  let sends = 0;
  session.agent = {
    async send() {
      sends++;
      await sendGate;
      return {
        async wait() { return { status: "finished", result: "final answer" }; },
        async cancel() {},
      };
    },
    async close() {},
  };
  const body = neutralBody({
    sessionId: session.id,
    model: "cursor-grok-4.5",
    input: {
      type: "user",
      text: "one replay-safe request",
      clientMessageId: "ccm2_fresh_active_frames",
    },
  });
  const first = new MockResponse();
  const firstRun = handleTurn(request(), first, body, "cursor-key");
  await waitFor(() => sends === 1 && session.sendPending && session.activeRes === first, "fresh active response");
  session.sse({ type: "reasoning", delta: "reason-one" });
  session.sse({ type: "text", delta: "text-two" });
  session.sse({ type: "tool_call", id: "call-three", name: "read", input: { path: "x" } });

  const retry = new MockResponse();
  const retryRun = handleTurn(request(), retry, body, "cursor-key");
  await waitFor(() => retry.text().includes("active_turn_handoff"), "active frame replay handoff");
  releaseSend();
  await Promise.all([firstRun, retryRun]);

  assert.equal(sends, 1);
  const replay = retry.text();
  const reasoningAt = replay.indexOf('"type":"reasoning","delta":"reason-one"');
  const textAt = replay.indexOf('"type":"text","delta":"text-two"');
  const toolAt = replay.indexOf('"type":"tool_call","id":"call-three"');
  assert.ok(reasoningAt >= 0, replay);
  assert.ok(textAt > reasoningAt, replay);
  assert.ok(toolAt > textAt, replay);
  assert.equal(replay.match(/reason-one/g)?.length, 1);
  assert.equal(replay.match(/text-two/g)?.length, 1);
  assert.equal(replay.match(/call-three/g)?.length, 1);
});

test("ambiguous fresh send retries the exact persisted envelope, agent, key, and tool snapshot after restart", async () => {
  const cursorKey = "fresh-uncertain-key";
  const session = new Session("fresh-uncertain-restart", cursorKey);
  session.clientEnv = { workspaceUnknown: true };
  session.seeded = true;
  sessions.set(session.id, session);
  const sends = [];
  let attempt = 0;
  const agent = {
    async send(message, options) {
      attempt++;
      const active = sessions.get(session.id);
      sends.push({
        agentId: active.agentId,
        advertised: active.advertise.map((tool) => tool.name),
        key: options.idempotencyKey,
        message,
      });
      if (attempt === 1) throw new Error("connection closed after fresh request write");
      return {
        async wait() { return { status: "finished", result: "fresh retry recovered" }; },
        async cancel() {},
      };
    },
    async close() {},
  };
  session.agent = agent;
  const firstBody = neutralBody({
    ...authoritativeToolsBody(advertised("OriginalTool"), { toolChoice: "auto" }),
    sessionId: session.id,
    model: "cursor-grok-4.5",
    input: {
      type: "user",
      text: "perform this fresh task once",
      system: "original system",
      history: "original history",
      clientMessageId: "ccm2_fresh_uncertain",
    },
  });
  const first = new MockResponse();
  await handleTurn(request(), first, firstBody, cursorKey);
  await waitFor(() => first.ended, "ambiguous fresh failure terminal");
  assert.equal(sends.length, 1);

  const originalAgentId = session.agentId;
  sessions.delete(session.id);
  platforms.set(keyHash(cursorKey), {
    promise: Promise.resolve({
      async resumeAgent(agentId) {
        assert.equal(agentId, originalAgentId);
        return agent;
      },
      async getAgentMessages() { return []; },
    }),
    stateRoot: TEST_STATE_ROOT,
    lastUsed: Date.now(),
    fp: keyFingerprint(cursorKey),
  });
  const retryBody = neutralBody({
    ...authoritativeToolsBody(advertised("NewlyRegisteredTool"), { toolChoice: "auto" }),
    sessionId: session.id,
    model: "cursor-grok-4.5",
    input: {
      type: "user",
      text: "perform this fresh task once",
      system: "original system",
      history: "original history",
      clientMessageId: "ccm2_fresh_uncertain",
    },
  });
  const retry = new MockResponse();
  await handleTurn(request(), retry, retryBody, cursorKey);
  await waitFor(() => retry.ended, "fresh exact retry terminal");

  assert.match(retry.text(), /fresh retry recovered/);
  assert.equal(sends.length, 2);
  assert.equal(sends[0].key, sends[1].key);
  assert.deepEqual(sends[0].message, sends[1].message);
  assert.deepEqual(sends[0].advertised, ["OriginalTool"]);
  assert.deepEqual(sends[1].advertised, ["OriginalTool"]);
  assert.deepEqual(sessions.get(session.id).advertise.map((tool) => tool.name), ["NewlyRegisteredTool"]);
});

test("a duplicate pure tool-result continuation hands off the resumed answer instead of short-acking", async () => {
  const { session } = seedSession("pure-continuation-handoff", "cursor-key");
  session.clientLeaseToken = "73";
  const opened = await openTool(session);
  session.activeRes = null;
  session.responseWriter = null;
  let finishRun;
  const runResult = new Promise((resolve) => { finishRun = resolve; });
  session.run = {
    wait() { return runResult; },
    async cancel() {},
  };
  void runResult.then((result) => session.onRunComplete(result));
  const result = { toolCallId: opened.call.wireId, content: "tool completed" };
  const input = { clientMessageId: "ccm2_pure_continuation_handoff" };
  const first = new MockResponse();
  const firstHandling = handleContinue(request(), first, continuationBody([result], input), "cursor-key");
  await waitFor(() => session.activeClientMessageId === input.clientMessageId && opened.round.callbacks.size === 0,
    "pure continuation resumed turn");

  const retry = new MockResponse();
  const retryHandling = handleContinue(request(), retry, continuationBody([result], input), "cursor-key");
  await waitFor(() => retry.text().includes("active_turn_handoff"), "pure continuation response handoff");
  finishRun({ status: "finished", result: "the resumed final answer" });
  await Promise.all([firstHandling, retryHandling]);

  assert.equal((await opened.promise).content, "tool completed");
  assert.match(retry.text(), /active_turn_handoff/);
  assert.match(retry.text(), /the resumed final answer/);
  assert.match(retry.text(), /"clientLease":\{"sessionId":"pure-continuation-handoff","token":"73","terminal":true\}/);
  assert.doesNotMatch(retry.text(), /already_applied|duplicate_or_concurrent/);
});

test("completed ccm2 fresh retries replay across restart while a new invocation stays distinct", async () => {
  const cursorKey = "cursor-key";
  const sessionId = "fresh-completed-replay";
  let sends = 0;
  const sendKeys = [];
  const agent = {
    async send(_message, options) {
      sends++;
      sendKeys.push(options.idempotencyKey);
      return {
        async wait() { return { status: "finished", result: `completed response ${sends}` }; },
        async cancel() {},
      };
    },
    async close() {},
  };
  platforms.set(keyHash(cursorKey), {
    promise: Promise.resolve({
      async resumeAgent() { throw new Error("agent not found"); },
      async createAgent() { return agent; },
      async getAgentMessages() { return []; },
    }),
    stateRoot: TEST_STATE_ROOT,
    lastUsed: Date.now(),
    fp: keyFingerprint(cursorKey),
  });
  const body = neutralBody({
    sessionId,
    model: "cursor-grok-4.5",
    input: { type: "user", text: "one completed request", clientMessageId: "ccm2_fresh_completed" },
  });
  const first = new MockResponse();
  await handleTurn(request(), first, body, cursorKey);
  await waitFor(() => first.ended, "completed fresh response");
  assert.equal(first.status, 200);
  assert.match(first.text(), /completed response 1/);
  assert.equal(sends, 1);

  const retry = new MockResponse();
  await handleTurn(request(), retry, body, cursorKey);
  await waitFor(() => retry.ended, "same invocation retry response");
  assert.equal(retry.status, 200);
  assert.match(retry.text(), /completed_turn_replay/);
  assert.match(retry.text(), /completed response 1/);
  assert.equal(sends, 1);
  assert.match(sendKeys[0], /_g1_/);

  const independentBody = neutralBody({
    sessionId,
    model: "cursor-grok-4.5",
    input: {
      type: "user",
      text: "one completed request",
      clientMessageId: "ccm2_fresh_completed",
      invocationId: "invocation-fresh-completed-0002",
    },
  });
  const independent = new MockResponse();
  await handleTurn(request(), independent, independentBody, cursorKey);
  await waitFor(() => independent.ended, "independent identical invocation response");
  assert.match(independent.text(), /completed response 2/);
  assert.doesNotMatch(independent.text(), /completed_turn_replay/);
  assert.equal(sends, 2);
  assert.notEqual(sendKeys[0], sendKeys[1]);
  assert.match(sendKeys[1], /_g1_invocation-fresh-completed-0002$/);

  const live = sessions.get(sessionId);
  sessions.delete(sessionId);
  await live.cancel({ terminalReason: "test_cleanup", detail: "simulate bridge restart" });
  completedTurnReceipts.clear();
  const coldRetry = new MockResponse();
  await handleTurn(request(), coldRetry, body, cursorKey);
  await waitFor(() => coldRetry.ended, "cold same-invocation retry response");
  assert.equal(coldRetry.status, 200);
  assert.match(coldRetry.text(), /completed_turn_replay/);
  assert.match(coldRetry.text(), /completed response 1/);
  assert.equal(sends, 2);
});

test("one explicit fresh invocation cannot be rebound to a different payload", async () => {
  const cursorKey = "fresh-invocation-binding-key";
  const sessionId = "fresh-invocation-binding";
  let sends = 0;
  const agent = {
    async send() {
      sends++;
      return {
        async wait() { return { status: "finished", result: "bound response" }; },
        async cancel() {},
      };
    },
    async close() {},
  };
  platforms.set(keyHash(cursorKey), {
    promise: Promise.resolve({
      async resumeAgent() { throw new Error("agent not found"); },
      async createAgent() { return agent; },
      async getAgentMessages() { return []; },
    }),
    stateRoot: TEST_STATE_ROOT,
    lastUsed: Date.now(),
    fp: keyFingerprint(cursorKey),
  });
  const base = neutralBody({
    sessionId,
    model: "cursor-grok-4.5",
    input: {
      type: "user",
      text: "first immutable payload",
      clientMessageId: "ccm2_explicit_fresh_binding",
      invocationId: "invocation-explicit-fresh-binding-0001",
    },
  });
  const first = new MockResponse();
  await handleTurn(request(), first, base, cursorKey);
  await waitFor(() => first.ended, "bound fresh completion");
  assert.equal(sends, 1);

  const conflicting = new MockResponse();
  await handleTurn(request(), conflicting, {
    ...base,
    input: { ...base.input, text: "different payload under the same invocation" },
  }, cursorKey);
  assert.equal(conflicting.status, 409);
  assert.equal(conflicting.json().error.code, "invocation_payload_conflict");
  assert.equal(sends, 1, "conflicting invocation reuse must not reach the SDK");
});

test("signed continuation authority ignores advisory session ids and binds before journal mutation", async () => {
  const cursorKey = "continuation-invocation-binding-key";
  const { session } = seedSession("continuation-invocation-binding", cursorKey);
  const opened = await openTool(session);
  const accepted = { toolCallId: opened.call.wireId, content: "authoritative result", isError: false };
  opened.round.applyResults([accepted]);
  await opened.promise;
  assert.equal(opened.round.terminal.reason, "completed");
  let sends = 0;
  session.seeded = true;
  session.agent = {
    async send() {
      sends++;
      return {
        async wait() { return { status: "finished", result: "continued exactly once" }; },
        async cancel() {},
      };
    },
    async close() {},
  };
  const input = {
    userText: "continue from the signed round",
    history: "bounded history",
    clientMessageId: "ccm2_explicit_continuation_binding",
    invocationId: "invocation-explicit-continuation-binding-0001",
  };
  const first = new MockResponse();
  await handleContinue(request(), first, continuationBody([accepted], input, {
    sessionId: "advisory-session-is-wrong",
  }), cursorKey);
  await waitFor(() => first.ended, "bound continuation completion");
  assert.equal(sends, 1);
  assert.match(first.text(), /continued exactly once/);
  const before = journalRecord(opened.round.route);

  const replay = new MockResponse();
  await handleContinue(request(), replay, continuationBody([accepted], input, {
    sessionId: "another-wrong-advisory-session",
  }), cursorKey);
  assert.equal(replay.status, 200);
  assert.match(replay.text(), /completed_turn_replay/);
  assert.equal(sends, 1);
  assert.deepEqual(journalRecord(opened.round.route), before);

  const conflict = new MockResponse();
  await handleContinue(request(), conflict, continuationBody([accepted], {
    ...input,
    userText: "different intent under the same invocation",
  }, { sessionId: session.id }), cursorKey);
  assert.equal(conflict.status, 200);
  assert.match(conflict.text(), /"errorCode":"invocation_payload_conflict"/);
  assert.match(conflict.text(), /continuation_conflict_contained/);
  assert.equal(sends, 1);
  assert.deepEqual(
    journalRecord(opened.round.route),
    before,
    "invocation conflicts must be rejected before any ToolRound mutation",
  );
});

test("completed receipts replay ordered reasoning, text, tool frames, usage, and terminal metadata", async () => {
  const cursorKey = "cursor-key";
  const sessionId = "ordered-completed-replay";
  const input = {
    type: "user",
    text: "persist every replay frame",
    clientMessageId: "ccm2_ordered_replay",
  };
  const session = new Session(sessionId, cursorKey);
  session.activeClientMessageId = input.clientMessageId;
  session.activeClientMessageHash = completedTurnRequestHash(input);
  session.activeClientMessageGeneration = 1;
  session.activeClientMessageKind = "fresh";
  session.activeIdentityPolicy = "legacy-client-message-v1";
  session.beginReplaySegment(input.clientMessageId);
  session.turnToken = 1;
  const live = new MockResponse();
  session.beginResponse(live);
  session.reasonedThisRun = true;
  session.streamedText = "visible answer";
  session.sse({ type: "reasoning", delta: "reason first" });
  session.sse({ type: "text", delta: "visible answer" });
  session.sse({ type: "tool_call", id: "call_replayed", name: "Lookup", input: { q: "ordered" } });
  session.onRunComplete({
    status: "finished",
    result: "visible answer",
    usage: { input_tokens: 11, output_tokens: 7 },
  });

  const file = exactTurnReceiptFile(cursorKey, sessionId, input.clientMessageId, input);
  const persisted = JSON.parse(readFileSync(file, "utf8"));
  assert.equal(persisted.version, 6);
  assert.equal(persisted.acceptancePhase, "COMPLETED");
  assert.deepEqual(persisted.events.map((event) => event.type), [
    "reasoning",
    "text",
    "tool_call",
    "turn_end",
  ]);
  assert.deepEqual(persisted.events.at(-1).usage, { input_tokens: 11, output_tokens: 7 });

  const replay = new MockResponse();
  await handleTurn(request(), replay, neutralBody({ sessionId, input }), cursorKey);
  assert.equal(replay.status, 200);
  assert.match(replay.text(), /completed_turn_replay/);
  const reasoningAt = replay.text().indexOf('"type":"reasoning"');
  const textAt = replay.text().indexOf('"type":"text"');
  const toolAt = replay.text().indexOf('"type":"tool_call"');
  const terminalAt = replay.text().indexOf('"type":"turn_end"');
  assert.ok(reasoningAt < textAt && textAt < toolAt && toolAt < terminalAt);
  assert.match(replay.text(), /"input_tokens":11/);
});

test("a non-durable completion emits delivery-unknown error instead of clean success", () => {
  const input = {
    type: "user",
    text: "this answer cannot be committed",
    clientMessageId: "ccm2_non_durable_completion",
  };
  const session = new Session("non-durable-completion", "cursor-key");
  session.activeClientMessageId = input.clientMessageId;
  session.activeClientMessageHash = completedTurnRequestHash(input);
  session.activeClientMessageGeneration = 1;
  session.activeClientMessageKind = "fresh";
  session.activeIdentityPolicy = "legacy-client-message-v1";
  session.beginReplaySegment(input.clientMessageId);
  session.replayEventOverflow = true;
  session.turnToken = 1;
  const response = new MockResponse();
  session.beginResponse(response);
  session.onRunComplete({ status: "finished", result: "model work happened", usage: {} });

  assert.match(response.text(), /"receipt":"completion_not_durable"/);
  assert.match(response.text(), /"deliveryUnknown":true/);
  assert.match(response.text(), /"stop_reason":"error"/);
  assert.doesNotMatch(response.text(), /"stop_reason":"end_turn"/);
  assert.equal(completedTurnReceipts.size, 0);
});

test("a post-acceptance fresh run failure retains one frozen generation across retry", async () => {
  const cursorKey = "cursor-key";
  const sessionId = "failed-attempt-fallback";
  const invocationId = "invocation-failed-attempt-0001";
  const input = {
    type: "user",
    text: "retry only after a proven failure",
    clientMessageId: "ccm2_failed_attempt",
    invocationId,
    history: "bounded history",
  };
  const sendKeys = [];
  let sends = 0;
  const session = new Session(sessionId, cursorKey);
  session.seeded = true;
  session.model = "cursor-grok-4.5";
  session.agent = {
    async send(_message, options) {
      sendKeys.push(options.idempotencyKey);
      sends++;
      const current = sends;
      return {
        async wait() {
          if (current === 1) throw new Error("post-acceptance upstream run failure");
          return { status: "finished", result: "exact retry succeeded", usage: {} };
        },
        async cancel() {},
      };
    },
    async close() {},
  };
  sessions.set(sessionId, session);

  const first = new MockResponse();
  await handleTurn(request(), first, neutralBody({
    sessionId,
    model: "cursor-grok-4.5",
    input,
  }), cursorKey);
  await waitFor(() => first.ended, "failed attempt terminal");
  assert.match(first.text(), /post-acceptance upstream run failure/);
  const file = exactTurnReceiptFile(cursorKey, sessionId, invocationId, input);
  const unresolved = JSON.parse(readFileSync(file, "utf8"));
  assert.equal(unresolved.acceptancePhase, "ACCEPTED");
  assert.equal(unresolved.generation, 1);

  const retry = new MockResponse();
  await handleTurn(request(), retry, neutralBody({
    sessionId,
    model: "composer-2.5",
    input,
  }), cursorKey);
  await waitFor(() => retry.ended, "exact retry terminal");
  assert.match(retry.text(), /exact retry succeeded/);
  assert.equal(sendKeys.length, 2);
  assert.match(sendKeys[0], /_g1_/);
  assert.equal(sendKeys[1], sendKeys[0]);
  const completed = JSON.parse(readFileSync(file, "utf8"));
  assert.equal(completed.status, "completed");
  assert.equal(completed.generation, 1);
});

test("cross-process receipt races elect one fresh owner and completed state is absorbing", async (t) => {
  const raceRoot = mkdtempSync(path.join(tmpdir(), "cursor-receipt-race-"));
  t.after(() => rmSync(raceRoot, { recursive: true, force: true }));
  const moduleUrl = new URL("./cursor-agent-bridge.mjs", import.meta.url).href;
  const cursorKey = "receipt-race-key";
  const sessionId = "receipt-race-session";
  const clientMessageId = "ccm2_receipt_race";
  const requestHash = "a".repeat(64);

  const runChild = (source) => new Promise((resolve, reject) => {
    const child = spawn(process.execPath, ["--input-type=module", "--eval", source], {
      cwd: path.dirname(new URL(import.meta.url).pathname),
      env: { ...process.env, CURSOR_AGENT_STATE_ROOT: raceRoot },
      stdio: ["ignore", "pipe", "pipe"],
    });
    let stdout = "";
    let stderr = "";
    child.stdout.on("data", (chunk) => { stdout += chunk; });
    child.stderr.on("data", (chunk) => { stderr += chunk; });
    child.once("error", reject);
    child.once("exit", (code, signal) => resolve({ code, signal, stdout, stderr }));
  });
  const waitUntil = (at) => `
    const view = new Int32Array(new SharedArrayBuffer(4));
    while (Date.now() < ${at}) Atomics.wait(view, 0, 0, Math.min(20, ${at} - Date.now()));
  `;
  const freshSource = (at, label) => `
    const bridge = await import(${JSON.stringify(moduleUrl)});
    ${waitUntil(at)}
    try {
      bridge.writeFreshDeliveryReceipt(
        ${JSON.stringify(cursorKey)},
        ${JSON.stringify(sessionId)},
        ${JSON.stringify(clientMessageId)},
        ${JSON.stringify(requestHash)},
        {
          generation: 1,
          agentId: "agent-receipt-race",
          idempotencyKey: "cctsend1_receipt-race",
          message: "immutable request",
          advertise: [],
          model: "composer-2.5",
          toolChoice: "",
          seededSystem: "",
          systemBlockIds: [],
          hasImages: false,
          identityPolicy: "legacy-client-message-v1",
        },
      );
      process.stdout.write("WIN:${label}\\n");
    } catch (error) {
      process.stdout.write("LOSS:${label}:" + (error.code || error.message) + "\\n");
    }
  `;
  const freshAt = Date.now() + 1000;
  const fresh = await Promise.all([
    runChild(freshSource(freshAt, "a")),
    runChild(freshSource(freshAt, "b")),
  ]);
  for (const child of fresh) {
    assert.deepEqual({ code: child.code, signal: child.signal }, { code: 0, signal: null }, child.stderr);
  }
  const freshOutput = fresh.map((child) => child.stdout.trim());
  assert.equal(freshOutput.filter((line) => line.startsWith("WIN:")).length, 1, freshOutput.join(" | "));
  assert.equal(freshOutput.filter((line) => line.startsWith("LOSS:")).length, 1, freshOutput.join(" | "));

  const receiptDir = path.join(raceRoot, ".cct-completed-turns");
  let receiptFiles = readdirSync(receiptDir).filter((name) => /^[a-f0-9]{64}\.json$/.test(name));
  assert.equal(receiptFiles.length, 1);
  let receipt = JSON.parse(readFileSync(path.join(receiptDir, receiptFiles[0]), "utf8"));
  assert.equal(receipt.acceptancePhase, "PREPARED_DURABLE");

  // Cross the send boundary before racing acceptance vs completion.
  const maybeAt = Date.now() + 200;
  const maybeChild = await runChild(`
    const bridge = await import(${JSON.stringify(moduleUrl)});
    ${waitUntil(maybeAt)}
    bridge.transitionAcceptancePhase(
      ${JSON.stringify(cursorKey)}, ${JSON.stringify(sessionId)},
      ${JSON.stringify(clientMessageId)}, ${JSON.stringify(requestHash)}, "MAYBE_ACCEPTED",
    );
  `);
  assert.deepEqual({ code: maybeChild.code, signal: maybeChild.signal }, { code: 0, signal: null }, maybeChild.stderr);
  receipt = JSON.parse(readFileSync(path.join(receiptDir, receiptFiles[0]), "utf8"));
  assert.equal(receipt.acceptancePhase, "MAYBE_ACCEPTED");

  const mutationAt = Date.now() + 1000;
  const transitionSource = `
    const bridge = await import(${JSON.stringify(moduleUrl)});
    ${waitUntil(mutationAt)}
    bridge.transitionAcceptancePhase(
      ${JSON.stringify(cursorKey)}, ${JSON.stringify(sessionId)},
      ${JSON.stringify(clientMessageId)}, ${JSON.stringify(requestHash)}, "ACCEPTED",
      { evidence: "agent_send_resolved" },
    );
  `;
  const completionSource = `
    const bridge = await import(${JSON.stringify(moduleUrl)});
    ${waitUntil(mutationAt)}
    bridge.writeCompletedTurnReceipt(
      ${JSON.stringify(cursorKey)}, ${JSON.stringify(sessionId)},
      ${JSON.stringify(clientMessageId)}, ${JSON.stringify(requestHash)},
      [{ type: "text", delta: "durable answer" }, { type: "turn_end", stop_reason: "end_turn" }],
      { input_tokens: 1, output_tokens: 1 },
      { generation: 1, replace: true, requestKind: "fresh", identityPolicy: "legacy-client-message-v1" },
    );
  `;
  const mutations = await Promise.all([runChild(transitionSource), runChild(completionSource)]);
  for (const child of mutations) {
    assert.deepEqual({ code: child.code, signal: child.signal }, { code: 0, signal: null }, child.stderr);
  }
  receiptFiles = readdirSync(receiptDir).filter((name) => /^[a-f0-9]{64}\.json$/.test(name));
  assert.equal(receiptFiles.length, 1);
  receipt = JSON.parse(readFileSync(path.join(receiptDir, receiptFiles[0]), "utf8"));
  assert.equal(receipt.status, "completed", "a stale state transition must never overwrite completion");
  assert.equal(readdirSync(receiptDir).some((name) => name.includes(".lock")), false);
});

test("acceptance-unknown fresh receipts survive terminal TTL and count cleanup", async () => {
  const cursorKey = "cursor-key";
  const sessionId = "unknown-receipt-retention";
  const invocationId = "invocation-unknown-retention-0001";
  const input = {
    type: "user",
    text: "acceptance is unknown",
    clientMessageId: "ccm2_unknown_retention",
    invocationId,
  };
  const session = new Session(sessionId, cursorKey);
  session.seeded = true;
  session.agent = {
    async send() { throw new Error("ambiguous agent.send transport rejection"); },
    async close() {},
  };
  sessions.set(sessionId, session);
  const response = new MockResponse();
  await handleTurn(request(), response, neutralBody({ sessionId, input }), cursorKey);
  await waitFor(() => response.ended, "ambiguous send terminal");

  const file = exactTurnReceiptFile(cursorKey, sessionId, invocationId, input);
  assert.equal(readFreshDeliveryReceipt(cursorKey, sessionId, invocationId, completedTurnRequestHash(input)).acceptancePhase, "MAYBE_ACCEPTED");
  cleanupCompletedTurnReceipts({ ttlMs: 1, maxTerminal: 0 });
  assert.equal(existsSync(file), true, "cleanup must retain unresolved acceptance evidence");

  const rollingUpgradeReceipt = JSON.parse(readFileSync(file, "utf8"));
  rollingUpgradeReceipt.status = "failed";
  rollingUpgradeReceipt.failedAt = Date.now() - 60_000;
  rollingUpgradeReceipt.failure = "legacy post-acceptance failure";
  writeFileSync(file, JSON.stringify(rollingUpgradeReceipt) + "\n");
  cleanupCompletedTurnReceipts({ ttlMs: 1, maxTerminal: 0 });
  assert.equal(existsSync(file), true, "legacy FAILED is still acceptance-unknown and must not be evicted");
});

test("reusing a client message id with different input cannot replay or deduplicate the wrong turn", async () => {
  const cursorKey = "cursor-key";
  const session = new Session("completed-reuse-different-input", cursorKey);
  session.clientEnv = { workspaceUnknown: true };
  session.seeded = true;
  const sends = [];
  const sendKeys = [];
  session.agent = {
    async send(message, options) {
      sends.push(String(message));
      sendKeys.push(options.idempotencyKey);
      const result = String(message).includes("second payload") ? "second answer" : "first answer";
      return {
        async wait() { return { status: "finished", result }; },
        async cancel() {},
      };
    },
    async close() {},
  };
  sessions.set(session.id, session);
  const firstBody = neutralBody({
    sessionId: session.id,
    input: { type: "user", text: "first payload", clientMessageId: "client-reused-id" },
  });
  const secondBody = neutralBody({
    sessionId: session.id,
    input: { type: "user", text: "second payload", clientMessageId: "client-reused-id" },
  });

  const first = new MockResponse();
  await handleTurn(request(), first, firstBody, cursorKey);
  await waitFor(() => first.ended, "first reused-id response");
  const second = new MockResponse();
  await handleTurn(request(), second, secondBody, cursorKey);
  await waitFor(() => second.ended, "second reused-id response");

  assert.match(first.text(), /first answer/);
  assert.match(second.text(), /second answer/);
  assert.doesNotMatch(second.text(), /completed_turn_replay/);
  assert.equal(sends.length, 2);
  assert.notEqual(sendKeys[0], sendKeys[1]);
});

test("SDK idempotency keys are scoped to the durable agent as well as the input", () => {
  const first = new Session("idempotency-scope-a", "cursor-key");
  const second = new Session("idempotency-scope-b", "cursor-key");
  const requestHash = completedTurnRequestHash({ type: "user", text: "same" });
  const firstKey = sdkSendIdempotencyKey(first, "same-client-id", requestHash);
  const retryKey = sdkSendIdempotencyKey(first, "same-client-id", requestHash);
  const secondKey = sdkSendIdempotencyKey(second, "same-client-id", requestHash);
  assert.equal(firstKey, retryKey);
  assert.notEqual(firstKey, secondKey);
});

test("fresh request hashes include system and history while ignoring JSON object key order", () => {
  const baseline = completedTurnRequestHash({
    type: "user",
    text: "same text",
    system: "system A",
    history: "history A",
    images: [{ mimeType: "image/png", data: "QUJD" }],
  });
  assert.equal(baseline, completedTurnRequestHash({
    images: [{ data: "QUJD", mimeType: "image/png" }],
    history: "history A",
    system: "system A",
    text: "same text",
    type: "user",
  }));
  assert.notEqual(baseline, completedTurnRequestHash({
    type: "user",
    text: "same text",
    system: "system B",
    history: "history A",
    images: [{ mimeType: "image/png", data: "QUJD" }],
  }));
  assert.notEqual(baseline, completedTurnRequestHash({
    type: "user",
    text: "same text",
    system: "system A",
    history: "history B",
    images: [{ mimeType: "image/png", data: "QUJD" }],
  }));
});

test("continuation request hashes cover canonical result content, errors, structure, images, system, and history", () => {
  const first = {
    toolCallId: "call-a",
    content: "alpha",
    isError: false,
    structuredContent: { nested: { value: 1 } },
    images: [{ data: "QUJD", mimeType: "image/png" }],
  };
  const second = { toolCallId: "call-b", content: "beta", isError: false };
  const input = {
    type: "tool_results",
    results: [first, second],
    userText: "continue",
    system: "system A",
    history: "history A",
    images: [{ url: "https://example.test/current.png", mimeType: "image/png" }],
  };
  const baseline = completedTurnRequestHash(input);
  assert.equal(baseline, completedTurnRequestHash({
    images: [{ mimeType: "image/png", url: "https://example.test/current.png" }],
    history: "history A",
    system: "system A",
    userText: "continue",
    results: [second, {
      images: [{ mimeType: "image/png", data: "QUJD" }],
      structuredContent: { nested: { value: 1 } },
      isError: false,
      content: "alpha",
      toolCallId: "call-a",
    }],
    type: "tool_results",
  }), "result order and JSON object key order are serialization details");
  assert.notEqual(baseline, completedTurnRequestHash({
    ...input,
    results: [{ ...first, content: "changed" }, second],
  }));
  assert.notEqual(baseline, completedTurnRequestHash({
    ...input,
    results: [{ ...first, isError: true }, second],
  }));
  assert.notEqual(baseline, completedTurnRequestHash({
    ...input,
    results: [{ ...first, structuredContent: { nested: { value: 2 } } }, second],
  }));
  assert.notEqual(baseline, completedTurnRequestHash({
    ...input,
    results: [{ ...first, images: [{ data: "REVG", mimeType: "image/png" }] }, second],
  }));
  assert.notEqual(baseline, completedTurnRequestHash({ ...input, system: "system B" }));
  assert.notEqual(baseline, completedTurnRequestHash({ ...input, history: "history B" }));
});

test("a supplied malformed invocation id is rejected instead of falling back to clientMessageId", async () => {
  assert.throws(
    () => turnInvocationIdentity({ invocationId: "bad id", clientMessageId: "ccm2_would_merge" }),
    /invocationId must be an opaque/,
  );
  const response = new MockResponse();
  await handleTurn(request(), response, neutralBody({
    sessionId: "malformed-invocation-id",
    input: {
      type: "user",
      text: "must not run",
      invocationId: "bad id",
      clientMessageId: "ccm2_would_merge",
    },
  }), "cursor-key");
  assert.equal(response.status, 422);
  assert.equal(sessions.has("malformed-invocation-id"), false);
});

test("a malformed exact fresh-delivery receipt fails closed before any SDK send", async (t) => {
  const cursorKey = "corrupt-fresh-receipt-key";
  const sessionId = "corrupt-fresh-receipt";
  const input = {
    type: "user",
    text: "do not duplicate this",
    clientMessageId: "ccm2_corrupt_fresh",
  };
  const file = exactTurnReceiptFile(cursorKey, sessionId, input.clientMessageId, input);
  mkdirSync(path.dirname(file), { recursive: true });
  writeFileSync(file, "{\"status\":\"delivering\",\"truncated\":true}\n");
  t.after(() => rmSync(file, { force: true }));

  const response = new MockResponse();
  await handleTurn(request(), response, neutralBody({ sessionId, input }), cursorKey);
  assert.equal(response.status, 503);
  assert.match(response.text(), /durable turn continuity state is unavailable/);
  assert.equal(sessions.has(sessionId), false);
});

test("a malformed exact continuation receipt cannot apply the tool result again", async (t) => {
  const cursorKey = "corrupt-continuation-receipt-key";
  const { session } = seedSession("corrupt-continuation-receipt", cursorKey);
  const opened = await openTool(session);
  const input = {
    type: "tool_results",
    results: [{ toolCallId: opened.call.wireId, content: "locally executed result" }],
    clientMessageId: "ccm2_corrupt_continuation",
  };
  const file = exactTurnReceiptFile(cursorKey, session.id, input.clientMessageId, input);
  mkdirSync(path.dirname(file), { recursive: true });
  writeFileSync(file, "not-json\n");
  t.after(() => rmSync(file, { force: true }));

  const response = new MockResponse();
  await handleContinue(request(), response, neutralBody({
    sessionId: session.id,
    model: "cursor-grok-4.5",
    input,
  }), cursorKey);
  assert.equal(response.status, 503);
  assert.equal(response.json().error.code, "durable_turn_receipt_unavailable");
  assert.equal(journalRecord(opened.round.route).calls[0].receipt, null);
});

test("unscoped legacy completed receipts never authorize a current hashed replay", () => {
  const cursorKey = "cursor-key";
  const sessionId = "legacy-receipt-scope";
  const clientMessageId = "same-client-id";
  const record = {
    version: 1,
    keyFingerprint: keyFingerprint(cursorKey),
    sessionId,
    clientMessageId,
    status: "completed",
    text: "old answer",
    usage: {},
  };
  const currentHash = completedTurnRequestHash({ type: "user", text: "different request" });
  assert.equal(validCompletedTurnReceipt(record, cursorKey, sessionId, clientMessageId, currentHash), false);
});

test("request-local replay failures are contained and never escape to process-wide unhandled rejection", async () => {
  const beforeHeaders = new MockResponse();
  const first = await handleHttpRequestSafely(request(), beforeHeaders, async () => {
    throw new Error("synthetic replay write failure");
  });
  assert.equal(first, false);
  assert.equal(beforeHeaders.status, 500);
  assert.equal(beforeHeaders.ended, true);

  const afterHeaders = new MockResponse();
  afterHeaders.headersSent = true;
  let destroyed = false;
  afterHeaders.destroy = () => { destroyed = true; };
  const second = await handleHttpRequestSafely(request(), afterHeaders, async () => {
    throw new Error("synthetic replay backpressure failure");
  });
  assert.equal(second, false);
  assert.equal(destroyed, true);
});

test("lost fresh response replays paused signed tool calls without a second SDK send", async () => {
  const { session } = seedSession("fresh-paused-replay", "cursor-key");
  const opened = await openTool(session);
  session.replayEvents = [
    { type: "text", delta: "same-text" },
    { type: "reasoning", delta: "ordered-reasoning" },
    { type: "text", delta: "same-text" },
    { type: "tool_call", id: opened.call.wireId, name: opened.call.name, input: opened.call.input },
  ];
  session.activeRes = null;
  session.responseWriter = null;
  session.activeClientMessageId = "ccm2_fresh_paused";
  session.activeClientMessageHash = completedTurnRequestHash({
    type: "user",
    text: "original request",
    clientMessageId: "ccm2_fresh_paused",
  });
  let cancels = 0;
  session.run = { async cancel() { cancels++; } };

  const replay = new MockResponse();
  await handleTurn(request(), replay, neutralBody({
    sessionId: session.id,
    model: "cursor-grok-4.5",
    input: { type: "user", text: "original request", clientMessageId: "ccm2_fresh_paused" },
  }), "cursor-key");
  assert.equal(replay.status, 200);
  assert.match(replay.text(), /paused_turn_replay/);
  assert.match(replay.text(), new RegExp(opened.call.wireId));
  assert.match(replay.text(), /"stop_reason":"tool_use"/);
  const replayText = replay.text();
  const firstText = replayText.indexOf('"delta":"same-text"');
  const reasoning = replayText.indexOf('"delta":"ordered-reasoning"');
  const secondText = replayText.indexOf('"delta":"same-text"', firstText + 1);
  const tool = replayText.indexOf(opened.call.wireId);
  assert.ok(firstText >= 0 && reasoning > firstText && secondText > reasoning && tool > secondText,
    "paused replay must preserve exact text/reasoning/tool order");
  assert.equal(replayText.indexOf('"delta":"same-text"', secondText + 1), -1,
    "paused replay must not synthesize a cumulative duplicate text frame");
  assert.equal(cancels, 0);

  await session.cancel({ terminalReason: "test_cleanup", detail: "paused replay complete" });
  await assert.rejects(opened.promise, /paused replay complete/);
});

test("all-legacy tool results recover from faithful replay and retain the replacement agent across restart", async () => {
  const cursorKey = "cursor-key";
  const sessionId = "legacy-unsigned-recovery";
  const sent = [];
  const createdAgentIds = [];
  const agent = {
    async send(message, options) {
      sent.push({ message, options });
      return {
        async wait() { return { status: "finished", result: "legacy work resumed" }; },
        async cancel() {},
      };
    },
    async close() {},
  };
  platforms.set(keyHash(cursorKey), {
    promise: Promise.resolve({
      async resumeAgent() { throw new Error("agent not found"); },
      async createAgent(options) {
        createdAgentIds.push(options.agentId);
        return agent;
      },
      async getAgentMessages() { return []; },
    }),
    stateRoot: TEST_STATE_ROOT,
    lastUsed: Date.now(),
    fp: keyFingerprint(cursorKey),
  });

  const results = [
    { toolCallId: "call_old_todo", content: "todo state retained" },
    { toolCallId: "call_old_job", content: "subagents were interrupted" },
  ];
  const input = {
    legacyUnsignedReplay: true,
    history: "ASSISTANT: [tool_calls: call_old_todo:todo({}); call_old_job:job({})]\nTOOL[call_old_todo]: todo state retained\nTOOL[call_old_job]: subagents were interrupted",
    historyFingerprint: "legacy-history-fingerprint",
    userText: "resume every interrupted subagent",
    clientMessageId: "ccm2_legacy_resume_once",
  };
  const response = new MockResponse();
  await handleContinue(request(), response, continuationBody(results, input, { sessionId }), cursorKey);
  await waitFor(() => response.ended, "legacy replay recovery terminal");

  assert.equal(response.status, 200);
  assert.equal(sent.length, 1);
  assert.match(String(sent[0].message), /call_old_todo/);
  assert.match(String(sent[0].message), /todo state retained/);
  assert.match(String(sent[0].message), /resume every interrupted subagent/);
  assert.match(sent[0].options.idempotencyKey, /^ccsend2_[A-Za-z0-9_-]{24}_[a-f0-9]{24}_g1_ccm2_legacy_resume_once$/);
  assert.equal(createdAgentIds.length, 1);
  assert.match(createdAgentIds[0], /^legacy-unsigned-recovery_legacy_[0-9a-f]{24}$/);

  const duplicate = new MockResponse();
  await handleContinue(request(), duplicate, continuationBody(results, input, { sessionId }), cursorKey);
  assert.equal(duplicate.status, 200);
  assert.match(duplicate.text(), /completed_turn_replay/);
  assert.match(duplicate.text(), /legacy work resumed/);
  assert.equal(sent.length, 1);

  const recovered = sessions.get(sessionId);
  assert.ok(recovered);
  const replacementAgentId = recovered.agentId;
  sessions.delete(sessionId);
  await recovered.cancel({ terminalReason: "test_cleanup", detail: "simulate bridge restart" });
  completedTurnReceipts.clear();
  platforms.clear();
  const coldDuplicate = new MockResponse();
  await handleContinue(request(), coldDuplicate, continuationBody(results, input, { sessionId }), cursorKey);
  assert.equal(coldDuplicate.status, 200);
  assert.match(coldDuplicate.text(), /completed_turn_replay/);
  assert.match(coldDuplicate.text(), /legacy work resumed/);
  assert.equal(sent.length, 1);
  const cold = new Session(sessionId, cursorKey);
  assert.equal(cold.agentId, replacementAgentId);
});

test("history replacement rotates before reseed and exact cold retry never resumes the stale agent", async () => {
  const cursorKey = "history-replacement-rotation-key";
  const sessionId = "history-replacement-rotation";
  const session = new Session(sessionId, cursorKey);
  const staleAgentId = session.agentId;
  let staleCloses = 0;
  session.agent = {
    async close() { staleCloses++; },
  };
  session.seeded = true;
  session.historyFingerprint = "history-before-replacement";
  session.model = "cursor-grok-4.5";
  sessions.set(sessionId, session);

  const resumed = [];
  const created = [];
  const sent = [];
  const replacementAgent = {
    async send(message) {
      sent.push(message);
      return {
        async wait() { return { status: "finished", result: "replacement context answer" }; },
        async cancel() {},
      };
    },
    async close() {},
  };
  platforms.set(keyHash(cursorKey), {
    promise: Promise.resolve({
      async resumeAgent(agentId) {
        resumed.push(agentId);
        throw new Error("agent not found");
      },
      async createAgent(options) {
        created.push(options.agentId);
        return replacementAgent;
      },
      async getAgentMessages() { return []; },
    }),
    stateRoot: TEST_STATE_ROOT,
    lastUsed: Date.now(),
    fp: keyFingerprint(cursorKey),
  });
  const body = neutralBody({
    sessionId,
    model: "cursor-grok-4.5",
    input: {
      type: "user",
      text: "answer from the compacted conversation",
      history: "complete bounded replacement transcript",
      historyFingerprint: "history-after-replacement",
      clientMessageId: "ccm2_history_replacement_rotation",
    },
  });
  const first = new MockResponse();
  await handleTurn(request(), first, body, cursorKey);
  await waitFor(() => first.ended, "replacement-context completion");
  const replacementAgentId = session.agentId;
  assert.notEqual(replacementAgentId, staleAgentId);
  assert.equal(staleCloses, 1);
  assert.deepEqual(resumed, [replacementAgentId]);
  assert.deepEqual(created, [replacementAgentId]);
  assert.equal(sent.length, 1);
  assert.match(sent[0], /complete bounded replacement transcript/);
  assert.equal(resumed.includes(staleAgentId), false);

  sessions.delete(sessionId);
  completedTurnReceipts.clear();
  const coldRetry = new MockResponse();
  await handleTurn(request(), coldRetry, body, cursorKey);
  assert.equal(coldRetry.status, 200);
  assert.match(coldRetry.text(), /completed_turn_replay/);
  assert.match(coldRetry.text(), /replacement context answer/);
  assert.equal(sent.length, 1, "an exact cold retry must not seed a second time");
  const cold = new Session(sessionId, cursorKey);
  assert.equal(cold.agentId, replacementAgentId, "cold construction must follow the durable replacement alias");
});

test("a failed durable alias publication cannot half-rotate a session onto a new credential", async (t) => {
  const session = new Session("atomic-key-rotation", "old-cursor-key");
  session.seeded = true;
  session.seededSystem = "old system";
  session.historyFingerprint = "old-history";
  const before = {
    agentId: session.agentId,
    cursorKey: session.cursorKey,
    keyEpoch: session.keyEpoch,
    seeded: session.seeded,
    seededSystem: session.seededSystem,
    historyFingerprint: session.historyFingerprint,
  };
  const newKey = "new-cursor-key";
  const scope = createHash("sha256")
    .update(keyFingerprint(newKey))
    .update("\0")
    .update(session.id)
    .digest("hex");
  const blockedAlias = path.join(TEST_STATE_ROOT, ".cct-agent-alias", `${scope}.json`);
  mkdirSync(blockedAlias, { recursive: true });
  t.after(() => rmSync(blockedAlias, { recursive: true, force: true }));

  await assert.rejects(
    session.rotateForKeyChange(newKey),
    /cannot persist durable Cursor agent alias/,
  );
  assert.deepEqual({
    agentId: session.agentId,
    cursorKey: session.cursorKey,
    keyEpoch: session.keyEpoch,
    seeded: session.seeded,
    seededSystem: session.seededSystem,
    historyFingerprint: session.historyFingerprint,
  }, before);
});

test("legacy recovery refuses missing replay and never downgrades a signed or mixed batch", async () => {
  const cursorKey = "cursor-key";
  const sessionId = "legacy-recovery-refusals";
  const missingReplay = new MockResponse();
  await handleContinue(request(), missingReplay, continuationBody([
    { toolCallId: "call_old", content: "completed" },
  ], {
    legacyUnsignedReplay: true,
    clientMessageId: "ccm2_missing_replay",
  }, { sessionId }), cursorKey);
  assert.equal(missingReplay.status, 410);
  assert.equal(missingReplay.json().error.code, "legacy_recovery_unavailable");
  assert.equal(sessions.has(sessionId), false);

  const mixed = new MockResponse();
  await handleContinue(request(), mixed, continuationBody([
    { toolCallId: "call_old", content: "legacy" },
    { toolCallId: "cct1_not_a_valid_signature", content: "signed" },
  ], {
    legacyUnsignedReplay: true,
    history: "call_old cct1_not_a_valid_signature",
    clientMessageId: "ccm2_mixed_strict",
  }, { sessionId }), cursorKey);
  assert.equal(mixed.status, 400);
  assert.equal(mixed.json().error.code, "invalid_tool_call_id");
  assert.equal(sessions.has(sessionId), false);
});

test("cold restart delivers a journaled mixed user turn exactly once after faithful result recovery", async () => {
  const cursorKey = "cursor-key";
  const { session } = seedSession("mixed-cold-restart", cursorKey);
  const opened = await openTool(session);
  opened.round.terminalize("shutdown", "bridge restarted");
  await assert.rejects(opened.promise, /shutdown/);
  liveToolRounds.delete(opened.round.route);
  sessions.delete(session.id);

  const sent = [];
  const agent = {
    async send(message) {
      sent.push(message);
      return {
        async wait() { return { status: "finished", result: "continued after restart" }; },
        async cancel() {},
      };
    },
    async close() {},
  };
  const platform = {
    async resumeAgent() { throw new Error("agent not found"); },
    async createAgent() { return agent; },
    async getAgentMessages() { return []; },
  };
  platforms.set(keyHash(cursorKey), {
    promise: Promise.resolve(platform),
    stateRoot: TEST_STATE_ROOT,
    lastUsed: Date.now(),
    fp: keyFingerprint(cursorKey),
  });

  const result = { toolCallId: opened.call.wireId, content: "durable local result", isError: false };
  const mixedInput = {
    history: "bounded prior conversation",
    userText: "continue this task after restart",
    clientMessageId: "ccm1_cold_restart_once",
    interruptRequested: true,
  };
  const response = new MockResponse();
  await handleContinue(request(), response, continuationBody([result], mixedInput), cursorKey);
  await waitFor(() => response.ended, "cold restart recovery terminal");
  await waitFor(
    () => journalRecord(opened.round.route).deferredInputs[0].state === "DELIVERED",
    "cold restart deferred input receipt",
  );
  assert.equal(response.status, 200);
  assert.equal(sent.length, 1);
  assert.match(sent[0], /durable local result/);
  assert.match(sent[0], /continue this task after restart/);
  assert.match(sent[0], new RegExp(opened.call.wireId));
  assert.ok(sent[0].endsWith("continue this task after restart"));
  assert.doesNotMatch(sent[0], /Continue the original task/);

  const duplicate = new MockResponse();
  await handleContinue(request(), duplicate, continuationBody([result], mixedInput), cursorKey);
  assert.equal(duplicate.status, 200);
  assert.match(duplicate.text(), /completed_turn_replay/);
  assert.match(duplicate.text(), /continued after restart/);
  assert.equal(sent.length, 1);
});

test("partial parallel results wait for every sibling and recover journaled user intent once", async () => {
  const cursorKey = "cursor-key";
  const { session } = seedSession("mixed-partial-parallel", cursorKey);
  const first = await openTool(session, { rawId: "parallel-first" });
  const secondPromise = session.openClientTool({
    source: "test",
    rawToolCallId: "parallel-second",
    name: "Lookup",
    input: { q: "second" },
    resultAdapter: (value) => value,
  });
  secondPromise.catch(() => {});
  await waitFor(() => first.round.fifo.length === 2, "second parallel call registration");
  assert.equal(session.flushJournaledCalls(), true);
  const second = first.round.calls.get(first.round.fifo[1]);
  await waitFor(() => second.handedAt != null, "second parallel call handoff");
  first.round.markAwaitingResults();
  session.activeRes = null;
  session.responseWriter = null;

  const sent = [];
  const agent = {
    async send(message) {
      sent.push(message);
      return {
        async wait() { return { status: "finished", result: "continued after partial batch" }; },
        async cancel() {},
      };
    },
    async close() {},
  };
  platforms.set(keyHash(cursorKey), {
    promise: Promise.resolve({
      async resumeAgent() { return agent; },
      async getAgentMessages() { return [{ role: "user" }]; },
    }),
    stateRoot: TEST_STATE_ROOT,
    lastUsed: Date.now(),
    fp: keyFingerprint(cursorKey),
  });

  const firstResult = { toolCallId: first.call.wireId, content: "first completed locally", isError: false };
  const mixedInput = {
    history: "bounded prior conversation",
    userText: "continue after both parallel calls complete",
    clientMessageId: "ccm1_partial_parallel_once",
    interruptRequested: true,
  };
  const partial = new MockResponse();
  await handleContinue(request(), partial, continuationBody([firstResult], mixedInput), cursorKey);
  assert.equal(partial.status, 200);
  assert.match(partial.text(), /partial_results_deferred_for_fidelity/);
  assert.match(partial.text(), /"stop_reason":"tool_use"/);
  assert.equal(first.round.pendingCount, 2, "no callback may resolve before every sibling is receipted");
  assert.equal(first.round.unreceiptedOwedCallCount, 1);
  assert.equal(sent.length, 0);

  const savedPartial = journalRecord(first.round.route);
  assert.equal(savedPartial.calls.find((call) => call.wireId === first.call.wireId).receipt.result.content,
    "first completed locally");
  assert.equal(savedPartial.calls.find((call) => call.wireId === second.wireId).receipt, null);
  assert.equal(savedPartial.deferredInputs[0].state, "QUEUED");

  const secondResult = { toolCallId: second.wireId, content: "second completed locally", isError: false };
  const final = new MockResponse();
  await handleContinue(request(), final, continuationBody([secondResult]), cursorKey);
  await waitFor(() => final.ended, "complete parallel recovery terminal");
  await assert.rejects(first.promise, /superseded by durable deferred user input/);
  await assert.rejects(secondPromise, /superseded by durable deferred user input/);

  const saved = journalRecord(first.round.route);
  const savedFirst = saved.calls.find((call) => call.wireId === first.call.wireId);
  const savedSecond = saved.calls.find((call) => call.wireId === second.wireId);
  assert.equal(savedFirst.receipt.result.content, "first completed locally");
  assert.equal(savedSecond.receipt.result.content, "second completed locally");
  assert.equal(saved.deferredInputs[0].state, "DELIVERED");
  assert.equal(sent.length, 1);
  assert.match(sent[0], /first completed locally/);
  assert.match(sent[0], /second completed locally/);
  assert.match(sent[0], /continue after both parallel calls complete/);
  assert.equal(final.status, 200);
  assert.match(final.text(), /continued after partial batch/);
});

test("a completed restart recovery without an answer receipt never fabricates retry success", async () => {
  const { session } = seedSession("recovery-idempotent", "cursor-key");
  const opened = await openTool(session);
  opened.round.terminalize("shutdown", "bridge restarted");
  await assert.rejects(opened.promise, /shutdown/);
  liveToolRounds.delete(opened.round.route);
  sessions.delete(session.id);

  const result = { toolCallId: opened.call.wireId, content: "executed once" };
  opened.round.commitResults([result], { allowTerminalReceipt: true });
  opened.round.recordRecovery({
    at: Date.now(),
    decision: "faithful_reseed",
    replacementAgentId: "replacement-agent",
    resultHashes: [opened.round.calls.get(opened.call.wireId).resultHash],
  });

  const replacement = new Session(opened.round.sessionId, "cursor-key");
  replacement.agentId = "replacement-agent";
  replacement.recoverySourceRound = opened.round;
  replacement.streamedText = "recovered answer";
  const output = new MockResponse();
  replacement.beginResponse(output);
  sessions.set(replacement.id, replacement);
  replacement.onRunComplete({ status: "finished", result: "recovered answer" });

  const saved = journalRecord(opened.round.route);
  assert.equal(saved.recovery.decision, "completed");
  assert.ok(saved.recovery.completedAt);

  const duplicate = new MockResponse();
  await handleContinue(request(), duplicate, continuationBody([result], { history: "bounded prior conversation" }), "cursor-key");
  assert.equal(duplicate.status, 200);
  assert.match(duplicate.text(), /completed_response_receipt_unavailable/);
  assert.match(duplicate.text(), /"stop_reason":"error"/);
  assert.doesNotMatch(duplicate.text(), /already_recovered/);
});

test("a failed restart recovery is journaled and releases its replacement session for retry", async () => {
  const { session } = seedSession("recovery-failed", "cursor-key");
  const opened = await openTool(session);
  opened.round.terminalize("shutdown", "bridge restarted");
  await assert.rejects(opened.promise, /shutdown/);
  liveToolRounds.delete(opened.round.route);
  sessions.delete(session.id);

  const result = { toolCallId: opened.call.wireId, content: "executed once" };
  opened.round.commitResults([result], { allowTerminalReceipt: true });
  opened.round.recordRecovery({ at: Date.now(), decision: "faithful_reseed", replacementAgentId: "failed-agent" });

  const replacement = new Session(opened.round.sessionId, "cursor-key");
  replacement.agentId = "failed-agent";
  replacement.recoverySourceRound = opened.round;
  const output = new MockResponse();
  replacement.beginResponse(output);
  sessions.set(replacement.id, replacement);
  replacement.onRunComplete({ status: "error", error: "replacement failed" });

  assert.equal(journalRecord(opened.round.route).recovery.decision, "failed");
  assert.equal(sessions.has(replacement.id), false);
  assert.match(output.text(), /"stop_reason":"error"/);
});

test("cancel terminal-resolves handed and registered calls exactly once", async () => {
  const { session } = seedSession("cancel", "key");
  const first = await openTool(session, { rawId: "first", awaiting: false });
  session.activeRes = null;
  session.responseWriter = null;
  const secondPromise = session.openClientTool({ source: "test", rawToolCallId: "second", name: "Lookup", input: {}, resultAdapter: (x) => x });
  secondPromise.catch(() => {});
  await waitFor(() => first.round.fifo.length === 2, "registered second call");
  const second = first.round.calls.get(first.round.fifo[1]);
  assert.equal(second.handedAt, null);
  await session.cancel({ terminalReason: "interrupted", detail: "user redirect" });
  assert.equal(first.round.state, "TERMINAL");
  assert.ok([...first.round.calls.values()].every((call) => call.state === "TERMINAL"));
  await assert.rejects(first.promise, /interrupted/);
  await assert.rejects(secondPromise, /interrupted/);
  assert.equal(first.round.terminal.reason, "interrupted");
});

test("a slow tail tool emitted after turn_end stays journaled in the same round and hands on the next response", async () => {
  const { session } = seedSession("slow-tail", "key");
  const first = await openTool(session, { rawId: "first" });
  first.round.batch = [];
  session.activeRes = null;
  session.responseWriter = null;

  const secondPromise = session.openClientTool({ source: "test", rawToolCallId: "tail", name: "Lookup", input: { q: "tail" }, resultAdapter: (value) => value });
  secondPromise.catch(() => {});
  await waitFor(() => first.round.fifo.length === 2, "journaled slow tail");
  const second = first.round.calls.get(first.round.fifo[1]);
  assert.equal(second.handedAt, null);
  assert.equal(second.wireId.split("_")[1], first.call.wireId.split("_")[1]);

  const nextResponse = new MockResponse();
  session.beginResponse(nextResponse);
  assert.equal(session.flushJournaledCalls(), true);
  await waitFor(() => second.handedAt != null, "slow tail handoff on next response");
  assert.match(nextResponse.text(), new RegExp(second.wireId));
  first.round.terminalize("client_cancelled", "cleanup");
  await assert.rejects(first.promise);
  await assert.rejects(secondPromise);
});

test("a queued tail is resolved as a typed non-execution when its client tool is removed before handoff", async () => {
  const { session } = seedSession("removed-slow-tail", "key");
  const first = await openTool(session, { rawId: "removed-first" });
  first.round.batch = [];
  session.activeRes = null;
  session.responseWriter = null;

  const tailPromise = session.openClientTool({
    source: "test",
    rawToolCallId: "removed-tail",
    name: "Lookup",
    input: { q: "tail" },
    resultAdapter: (value) => value,
  });
  await waitFor(() => first.round.fifo.length === 2, "removed tail registration");
  const changed = authoritativeToolsBody([], { toolChoice: "none" });
  refreshSessionFromBody(session, changed, prepareAdvertisedToolRegistry(changed));
  const nextResponse = new MockResponse();
  session.beginResponse(nextResponse);
  assert.equal(session.flushJournaledCalls(), true);
  const result = await tailPromise;
  assert.equal(result.isError, true);
  assert.equal(result.structuredContent.code, "client_tool_unavailable");
  assert.equal(result.structuredContent.executed, false);
  assert.doesNotMatch(nextResponse.text(), /"type":"tool_call"/);

  first.round.terminalize("client_cancelled", "cleanup");
  await assert.rejects(first.promise);
});

test("a queued tail is not handed under an incompatible replacement schema", async () => {
  const { session } = seedSession("changed-slow-tail", "key");
  const first = await openTool(session, { rawId: "changed-first" });
  first.round.batch = [];
  session.activeRes = null;
  session.responseWriter = null;
  const tailPromise = session.openClientTool({
    source: "test",
    rawToolCallId: "changed-tail",
    name: "Lookup",
    input: { q: "old shape" },
    resultAdapter: (value) => value,
  });
  await waitFor(() => first.round.fifo.length === 2, "changed tail registration");
  const changed = authoritativeToolsBody([{
    name: "Lookup",
    inputSchema: {
      type: "object",
      properties: { repo_path: { type: "string" } },
      required: ["repo_path"],
      additionalProperties: false,
    },
  }], { toolChoice: "auto" });
  refreshSessionFromBody(session, changed, prepareAdvertisedToolRegistry(changed));
  const nextResponse = new MockResponse();
  session.beginResponse(nextResponse);
  assert.equal(session.flushJournaledCalls(), true);
  const result = await tailPromise;
  assert.equal(result.isError, true);
  assert.equal(result.structuredContent.code, "client_tool_schema_changed");
  assert.equal(result.structuredContent.executed, false);
  assert.doesNotMatch(nextResponse.text(), /"type":"tool_call"/);

  first.round.terminalize("client_cancelled", "cleanup");
  await assert.rejects(first.promise);
});

test("a queued tail is refused when the same tool descriptor changes but old arguments still validate", async () => {
  const { session } = seedSession("semantic-change-slow-tail", "key");
  const first = await openTool(session, { rawId: "semantic-first" });
  first.round.batch = [];
  session.activeRes = null;
  session.responseWriter = null;
  const tailPromise = session.openClientTool({
    source: "test",
    rawToolCallId: "semantic-tail",
    name: "Lookup",
    input: { q: "still structurally valid" },
    resultAdapter: (value) => value,
  });
  await waitFor(() => first.round.fifo.length === 2, "semantic tail registration");
  const changed = authoritativeToolsBody([{
    name: "Lookup",
    description: "Same argument shape, but a materially different client-side operation",
    inputSchema: {
      type: "object",
      properties: { q: { type: "string", description: "new semantics" } },
      required: ["q"],
      additionalProperties: false,
    },
  }], { toolChoice: "auto" });
  refreshSessionFromBody(session, changed, prepareAdvertisedToolRegistry(changed));
  const nextResponse = new MockResponse();
  session.beginResponse(nextResponse);
  assert.equal(session.flushJournaledCalls(), true);
  const result = await tailPromise;
  assert.equal(result.structuredContent.code, "client_tool_schema_changed");
  assert.doesNotMatch(nextResponse.text(), /"type":"tool_call"/);

  first.round.terminalize("client_cancelled", "cleanup");
  await assert.rejects(first.promise);
});

test("new user input interrupts an awaiting round without relying on a deleted Session pending map", async () => {
  const { session } = seedSession("interrupt-path", "key");
  const opened = await openTool(session);
  session.run = { async cancel() {} };
  session.tail = new Promise(() => {});
  const res = new MockResponse();
  await handleTurn(request(), res, neutralBody({ sessionId: session.id, input: { type: "user", text: "new direction" } }), "key");
  assert.equal(res.status, 200);
  assert.equal(opened.round.terminal.reason, "interrupted");
  await assert.rejects(opened.promise, /interrupted/);
  sessions.delete(session.id);
});

test("a registered parallel sibling is handed before mixed input can redirect the round", async () => {
  const cursorKey = "cursor-key";
  const { session } = seedSession("registered-sibling-wave", cursorKey);
  const first = await openTool(session, { rawId: "registered-wave-first" });
  const secondPromise = session.openClientTool({
    source: "test",
    rawToolCallId: "registered-wave-second",
    name: "Lookup",
    input: { q: "second" },
    resultAdapter: (value) => value,
  });
  secondPromise.catch(() => {});
  await waitFor(() => first.round.fifo.length === 2, "registered sibling");
  const second = first.round.calls.get(first.round.fifo[1]);
  assert.equal(second.state, "REGISTERED");
  assert.equal(second.handedAt, null);
  assert.equal(first.round.unreceiptedOwedCallCount, 2,
    "both the handed first call and registered sibling are durable unreceipted obligations");

  session.activeRes = null;
  session.responseWriter = null;
  let staleRunCancels = 0;
  session.run = { async cancel() { staleRunCancels++; } };
  const originalAgentId = session.agentId;
  const response = new MockResponse();
  await handleContinue(request(), response, continuationBody([{
    toolCallId: first.call.wireId,
    content: "first result",
  }], {
    userText: "after both tools, SSH the zip",
    clientMessageId: "ccm2_registered_sibling_user",
    interruptRequested: true,
    history: "bounded prior conversation",
  }), cursorKey);

  await waitFor(() => second.handedAt != null, "registered sibling handoff");
  assert.equal(response.status, 200);
  assert.match(response.text(), new RegExp(second.wireId));
  assert.match(response.text(), /partial_results_deferred_for_fidelity/);
  assert.match(response.text(), /"stop_reason":"tool_use"/);
  assert.equal(first.round.unreceiptedOwedCallCount, 1);
  assert.equal(staleRunCancels, 0, "an incomplete callback wave must not cancel the SDK run");
  assert.equal(session.agentId, originalAgentId, "an incomplete callback wave must not rotate lineage");

  const sent = [];
  const recoveredAgent = {
    async send(message) {
      sent.push(message);
      return {
        async wait() { return { status: "finished", result: "SSH request resumed exactly once" }; },
        async cancel() {},
      };
    },
    async close() {},
  };
  platforms.set(keyHash(cursorKey), {
    promise: Promise.resolve({
      async resumeAgent() { return recoveredAgent; },
      async createAgent() { return recoveredAgent; },
      async getAgentMessages() { return []; },
    }),
    stateRoot: TEST_STATE_ROOT,
    lastUsed: Date.now(),
    fp: keyFingerprint(cursorKey),
  });

  const finalResponse = new MockResponse();
  const finalBody = continuationBody([{
    toolCallId: second.wireId,
    content: "second result",
  }]);
  await handleContinue(request(), finalResponse, finalBody, cursorKey);
  await waitFor(() => finalResponse.ended, "parallel recovery final response");
  assert.equal(sent.length, 1, "the deferred user instruction must execute once after the full sibling wave");
  assert.equal(typeof sent[0], "string");
  assert.match(sent[0], /first result/);
  assert.match(sent[0], /second result/);
  assert.ok(sent[0].indexOf("first result") < sent[0].indexOf("CURRENT USER MESSAGE"));
  assert.ok(sent[0].endsWith("after both tools, SSH the zip"),
    "recovered facts must precede the exact authoritative user suffix");
  assert.equal(sent[0].match(/after both tools, SSH the zip/g)?.length, 1);
  await assert.rejects(first.promise, /superseded by durable deferred user input/);
  await assert.rejects(secondPromise, /superseded by durable deferred user input/);

  const duplicate = new MockResponse();
  await handleContinue(request(), duplicate, finalBody, cursorKey);
  assert.equal(sent.length, 1, "a repeated final sibling receipt must replay, not redeliver user intent");
  assert.match(duplicate.text(), /completed_response_receipt_unavailable/);
  assert.match(duplicate.text(), /"stop_reason":"error"/);
});

test("a finished SDK run with an unresolved callback emits an error terminal, never end_turn", async () => {
  const { session, output } = seedSession("unfinished-callback", "key");
  const opened = await openTool(session);
  session.onRunComplete({ status: "finished", result: "" });
  await assert.rejects(opened.promise, /run_error/);
  assert.match(output.text(), /"stop_reason":"error"/);
  assert.doesNotMatch(output.text(), /"stop_reason":"end_turn"/);
  assert.equal(opened.round.terminal.reason, "run_error");
});

test("a clean finished SDK run omits the optional error field", () => {
  const { session, output } = seedSession("finished-without-error", "key", []);
  session.onRunComplete({ status: "finished", result: "complete" });
  assert.match(output.text(), /"stop_reason":"end_turn"/);
  assert.doesNotMatch(output.text(), /"error":/);
});

test("response disconnect after tool handoff terminal-resolves the round as transport_error", async () => {
  const { session, output } = seedSession("disconnect-after-handoff", "key");
  const opened = await openTool(session);
  output.emit("close");
  await assert.rejects(opened.promise, /transport_error/);
  assert.equal(opened.round.terminal.reason, "transport_error");
  assert.equal(opened.round.pendingCount, 0);
});

test("transport loss before tool-call handoff terminal-resolves a registered call without fake delivery", async () => {
  const session = new Session("disconnect-before-handoff", "key");
  session.clientEnv = { workspaceUnknown: true };
  session.advertise = advertised();
  const promise = session.openClientTool({
    source: "test",
    rawToolCallId: "not-handed",
    name: "Lookup",
    input: { q: "x" },
    resultAdapter: (value) => value,
  });
  promise.catch(() => {});
  await waitFor(() => session.currentRound?.fifo.length === 1, "registered unhanded call");
  const call = session.currentRound.calls.get(session.currentRound.fifo[0]);
  assert.equal(call.handedAt, null);
  await session.cancel({ terminalReason: "transport_error", detail: "client disconnected before handoff" });
  await assert.rejects(promise, /transport_error/);
  assert.equal(call.handedAt, null);
  assert.equal(session.currentRound.terminal.reason, "transport_error");
  assert.equal(session.currentRound.pendingCount, 0);
});

test("shutdown cancellation terminal-resolves every open callback", async () => {
  const { session } = seedSession("shutdown-cancel", "key");
  const opened = await openTool(session);
  await session.cancel({ terminalReason: "shutdown", detail: "test shutdown" });
  await assert.rejects(opened.promise, /shutdown/);
  assert.equal(opened.round.terminal.reason, "shutdown");
  assert.equal(opened.round.pendingCount, 0);
});

test("HTTP MCP tools/call uses ToolRound and preserves isError, structuredContent, and image output", async () => {
  const previous = process.env.CURSOR_COMPOSER_MCP_IMAGE_RESULTS;
  process.env.CURSOR_COMPOSER_MCP_IMAGE_RESULTS = "1";
  const { session } = seedSession("mcp-http", "key");
  try {
    const responsePromise = mcpDispatch({ jsonrpc: "2.0", id: 7, method: "tools/call", params: { name: "Lookup", arguments: { q: "x" } } }, session.id, DEFAULT_MCP_SERVER_KEY);
    await waitFor(() => session.currentRound?.fifo.length === 1 && session.currentRound.calls.get(session.currentRound.fifo[0]).handedAt, "HTTP MCP handoff");
    if (session.flushTimer) { clearTimeout(session.flushTimer); session.flushTimer = null; }
    const round = session.currentRound;
    const call = round.calls.get(round.fifo[0]);
    assert.equal(call.source, "http-mcp");
    round.markAwaitingResults();
    round.applyResults([{
      toolCallId: call.wireId,
      content: "failed",
      isError: true,
      structuredContent: { code: "E_LOOKUP" },
      images: [{ data: "QUJD", mimeType: "image/png" }],
    }]);
    const response = await responsePromise;
    assert.equal(response.result.isError, true);
    assert.deepEqual(response.result.structuredContent, { code: "E_LOOKUP" });
    assert.deepEqual(response.result.content, [
      { type: "text", text: "failed" },
      { type: "image", data: "QUJD", mimeType: "image/png" },
    ]);
  } finally {
    if (previous === undefined) delete process.env.CURSOR_COMPOSER_MCP_IMAGE_RESULTS;
    else process.env.CURSOR_COMPOSER_MCP_IMAGE_RESULTS = previous;
  }
});

test("HTTP MCP calls are server-scoped and equal JSON-RPC ids cannot collide across servers", async () => {
  const tools = [
    { name: "mcp__alpha__one", inputSchema: { type: "object", properties: { q: { type: "string" } }, required: ["q"] } },
    { name: "mcp__beta__two", inputSchema: { type: "object", properties: { q: { type: "string" } }, required: ["q"] } },
  ];
  const { session } = seedSession("mcp-server-scope", "key", tools);
  session.mcpServerKeys = [DEFAULT_MCP_SERVER_KEY, "alpha", "beta"];

  const wrongServer = await mcpDispatch({
    jsonrpc: "2.0", id: 9, method: "tools/call", params: { name: "mcp__beta__two", arguments: { q: "x" } },
  }, session.id, "alpha");
  assert.equal(wrongServer.result.isError, true);
  assert.match(wrongServer.result.content[0].text, /not available/);
  assert.equal(session.currentRound, null);

  const alpha = mcpDispatch({
    jsonrpc: "2.0", id: 1, method: "tools/call", params: { name: "mcp__alpha__one", arguments: { q: "a" } },
  }, session.id, "alpha");
  const beta = mcpDispatch({
    jsonrpc: "2.0", id: 1, method: "tools/call", params: { name: "mcp__beta__two", arguments: { q: "b" } },
  }, session.id, "beta");
  await waitFor(() => session.currentRound?.fifo.length === 2
    && session.currentRound.fifo.every((wireId) => session.currentRound.calls.get(wireId).handedAt), "two scoped MCP calls");
  const round = session.currentRound;
  const calls = round.fifo.map((wireId) => round.calls.get(wireId));
  assert.notEqual(calls[0].rawIdHash, calls[1].rawIdHash);
  assert.deepEqual(calls.map((call) => call.name).sort(), ["mcp__alpha__one", "mcp__beta__two"]);
  if (session.flushTimer) { clearTimeout(session.flushTimer); session.flushTimer = null; }
  round.markAwaitingResults();
  round.applyResults(calls.map((call) => ({ toolCallId: call.wireId, content: call.name, isError: false })));
  const responses = await Promise.all([alpha, beta]);
  assert.deepEqual(responses.map((response) => response.id), [1, 1]);
});

test("JSON-RPC tool batches dispatch sequentially and preserve response order", async () => {
  const { session } = seedSession("mcp-batch", "key", [
    { name: "A", inputSchema: { type: "object", properties: { q: { type: "string" } }, required: ["q"] } },
    { name: "B", inputSchema: { type: "object", properties: { q: { type: "string" } }, required: ["q"] } },
  ]);
  const pending = dispatchMcpBatch([
    { jsonrpc: "2.0", id: 11, method: "tools/call", params: { name: "A", arguments: { q: "a" } } },
    { jsonrpc: "2.0", id: 12, method: "tools/call", params: { name: "B", arguments: { q: "b" } } },
  ], session.id, DEFAULT_MCP_SERVER_KEY);
  await waitFor(() => session.currentRound?.fifo.length === 1
    && session.currentRound.calls.get(session.currentRound.fifo[0]).handedAt, "first sequential batch handoff");
  let round = session.currentRound;
  if (session.flushTimer) { clearTimeout(session.flushTimer); session.flushTimer = null; }
  round.markAwaitingResults();
  round.applyResults([{ toolCallId: round.fifo[0], content: "ok-a", isError: false }]);
  const firstRound = round;
  await waitFor(() => session.currentRound !== firstRound && session.currentRound?.fifo.length === 1
    && session.currentRound.calls.get(session.currentRound.fifo[0]).handedAt, "second sequential batch handoff");
  round = session.currentRound;
  if (session.flushTimer) { clearTimeout(session.flushTimer); session.flushTimer = null; }
  round.markAwaitingResults();
  round.applyResults([{ toolCallId: round.fifo[0], content: "ok-b", isError: false }]);
  const responses = await pending;
  assert.deepEqual(responses.map((response) => response.id), [11, 12]);
});

test("patched MCP dispatch uses the same ToolRound adapter and result contract", async () => {
  const { session } = seedSession("mcp-patched", "key");
  const responsePromise = session.dispatchMcp({ toolName: "Lookup", args: { q: "x" }, toolCallId: "sdk-mcp" });
  await waitFor(() => session.currentRound?.fifo.length === 1 && session.currentRound.calls.get(session.currentRound.fifo[0]).handedAt, "patched MCP handoff");
  if (session.flushTimer) { clearTimeout(session.flushTimer); session.flushTimer = null; }
  const round = session.currentRound;
  const call = round.calls.get(round.fifo[0]);
  assert.equal(call.source, "patched-mcp");
  round.markAwaitingResults();
  round.applyResults([{ toolCallId: call.wireId, content: "failed", isError: true, structuredContent: { code: "E_LOOKUP" } }]);
  const response = await responsePromise;
  assert.deepEqual(response.__ccJson, mcpDispatchResult("failed", true, null, { code: "E_LOOKUP" }));
});

test("patched and HTTP MCP render the same transformed authoritative blocks and scalar structured values", async () => {
  const run = async (transport, structuredContent) => {
    const { session } = seedSession(`mcp-block-transform-${transport}`, "key");
    const responsePromise = transport === "http"
      ? mcpDispatch({ jsonrpc: "2.0", id: 41, method: "tools/call", params: { name: "Lookup", arguments: { q: "x" } } }, session.id, DEFAULT_MCP_SERVER_KEY)
      : session.dispatchMcp({ toolName: "Lookup", args: { q: "x" }, toolCallId: `sdk-${transport}` });
    await waitFor(() => session.currentRound?.fifo.length === 1
      && session.currentRound.calls.get(session.currentRound.fifo[0]).handedAt, `${transport} transformed MCP handoff`);
    if (session.flushTimer) { clearTimeout(session.flushTimer); session.flushTimer = null; }
    const round = session.currentRound;
    const call = round.calls.get(round.fifo[0]);
    round.markAwaitingResults();
    const live = `running in background with id task_abc\n${"x".repeat(COMPOSER_LIVE_TOOL_RESULT_MAX_BYTES + 1024)}`;
    round.applyResults([{
      toolCallId: call.wireId,
      content: live,
      contentBlocks: [{ type: "text", text: live }],
      structuredContent,
      structuredContentPresent: true,
    }]);
    return responsePromise;
  };

  const http = await run("http", null);
  const httpText = http.result.content.map((part) => part.text || "").join("\n");
  assert.match(httpText, /\[BRIDGE\] STILL RUNNING/);
  assert.match(httpText, /truncated by proxy/);
  assert.match(httpText, /\[Structured content: null\]/);

  const patched = await run("patched", ["one", 2]);
  const patchedText = patched.__ccJson.success.content.map((part) => part.text && part.text.text || "").join("\n");
  assert.match(patchedText, /\[BRIDGE\] STILL RUNNING/);
  assert.match(patchedText, /truncated by proxy/);
  assert.match(patchedText, /\[Structured content: \["one",2\]\]/);
});

test("MCP missing session and unknown tool are typed errors, never fake success", async () => {
  const missing = await mcpDispatch({ jsonrpc: "2.0", id: 1, method: "tools/call", params: { name: "Lookup", arguments: {} } }, "gone", DEFAULT_MCP_SERVER_KEY);
  assert.equal(missing.result.isError, true);
  const { session } = seedSession("unknown-tool", "key", [{ name: "Read" }, { name: "Bash" }]);
  const unknown = await mcpDispatch({ jsonrpc: "2.0", id: 2, method: "tools/call", params: { name: "DestroyEverything", arguments: {} } }, session.id, DEFAULT_MCP_SERVER_KEY);
  assert.equal(unknown.result.isError, true);
  assert.match(unknown.result.content[0].text, /not available/);
});

test("MCP tools/list is derived live from the registry with object schemas", async () => {
  const { session } = seedSession("list", "key", [{ name: "A" }, { name: "B", inputSchema: { type: "object", required: ["q"] } }]);
  const listed = await mcpDispatch({ jsonrpc: "2.0", id: 3, method: "tools/list" }, session.id, DEFAULT_MCP_SERVER_KEY);
  assert.deepEqual(listed.result.tools.map((tool) => tool.name), ["A", "B"]);
  assert.deepEqual(listed.result.tools[0].inputSchema, { type: "object" });
  assert.deepEqual(mcpToolsForServer(session, DEFAULT_MCP_SERVER_KEY).map((tool) => tool.name), ["A", "B"]);
});

test("Grok 4.5 model aliases preserve slow default and map effort", () => {
  assert.deepEqual(composerModelSelection("cursor-grok-4.5"), { id: "grok-4.5", params: [{ id: "fast", value: "false" }, { id: "effort", value: "high" }] });
  assert.deepEqual(composerModelSelection("cursor-grok-4.5-fast-low"), { id: "grok-4.5", params: [{ id: "fast", value: "true" }, { id: "effort", value: "low" }] });
  assert.deepEqual(composerModelSelection("cursor-grok-4.5-xhigh"), { id: "grok-4.5", params: [{ id: "fast", value: "false" }, { id: "effort", value: "high" }] });
});

test("tool-choice gating does not widen the advertised set or fake native success", () => {
  const tools = advertised("Lookup");
  assert.deepEqual(effectiveAdvertise(tools, "none"), []);
  assert.deepEqual(effectiveAdvertise(tools, "specific:Missing"), []);
  assert.equal(nativeToolBlockedByChoice("none"), true);
  assert.equal(nativeToolBlockedByChoice("specific:Lookup"), true);
  assert.equal(nativeToolBlockedByChoice("auto"), false);
  const blocked = blockedNativeResult("readArgs", { path: "/x" }, "");
  assert.ok(blocked.__ccJson.error);
  assert.match(JSON.stringify(blocked), /disabled/);
  assert.match(constraintInstructions({ toolChoice: "required" }), /must call/i);
});

test("synthetic agent-tools artifacts are blocked before ToolRound across native, MCP, and shell seams", async () => {
  const artifact = "agent-tools/fdc5389e-988d-4ef4-8d1b-f31037f28f8a.txt";
  assert.equal(syntheticAgentArtifactRequest("Write", { file_path: artifact }), true);
  assert.equal(syntheticAgentArtifactRequest("mcp__client-tools__Read", { path: `/tmp/${artifact}` }), true);
  assert.equal(syntheticAgentArtifactRequest("Bash", { command: `head -c 2000 ${artifact}` }), true);
  assert.equal(syntheticAgentArtifactRequest("write_file", { file_path: artifact }), true);
  assert.equal(syntheticAgentArtifactRequest("read-file", { path: artifact }), true);
  assert.equal(syntheticAgentArtifactRequest("run_terminal_cmd", { command: `head ${artifact}` }), true);
  assert.equal(syntheticAgentArtifactRequest("Write", { file_path: "agent-tools/notes.txt" }), false);
  assert.equal(syntheticAgentArtifactRequest("Write", { file_path: "/home/me/repo/normal.txt" }), false);
  assert.equal(syntheticAgentArtifactFailure("Lookup", { path: artifact }), null);

  const { session, output } = seedSession("synthetic-artifact", "key", [
    { name: "Write", inputSchema: { type: "object", properties: { path: { type: "string" }, content: { type: "string" } } } },
    { name: "Read", inputSchema: { type: "object", properties: { path: { type: "string" } } } },
    { name: "Bash", inputSchema: { type: "object", properties: { command: { type: "string" } } } },
  ]);

  const direct = await session.openClientTool({ source: "test", rawToolCallId: "synthetic-direct", name: "Write", input: { path: artifact, content: "large result" }, resultAdapter: (value) => value });
  assert.equal(direct.isError, true);
  assert.match(direct.content, /already available|use it directly/i);
  assert.equal(session.currentRound, null, "a rejected internal artifact must not allocate a durable client ToolRound");
  assert.doesNotMatch(output.text(), /tool_call/);

  const native = await session.dispatchUnary("writeArgs", CC_CASES.writeArgs, { toolCallId: "native-write", path: artifact, fileText: "large result" });
  assert.ok(native.__ccJson.error, "native write receives the real typed filesystem error variant");
  assert.match(JSON.stringify(native), /agent-tools artifact handoff is disabled/);
  assert.equal(session.currentRound, null);

  const mcp = await session.dispatchMcp({ toolCallId: "mcp-write", toolName: "Write", args: { path: artifact, content: "large result" } });
  assert.equal(mcp.__ccJson.success.isError, true, "an MCP-aliased Write cannot bypass the same structural gate");
  assert.match(JSON.stringify(mcp), /already available|use it directly/i);
  assert.equal(session.currentRound, null);

  const chunks = [];
  for await (const chunk of session.dispatchStream("shellStreamArgs", CC_CASES.shellStreamArgs, { toolCallId: "native-shell", command: `ls ${artifact}` })) chunks.push(chunk.__ccJson);
  assert.equal(chunks.at(-1).exit.code, 1);
  assert.equal(chunks.at(-1).exit.aborted, true);
  assert.equal(session.currentRound, null);
});

test("SDK-dispatch preflight blocks reserved artifacts before generic client-tool routing", async () => {
  const artifact = "agent-tools/15428c99-00fa-4575-b194-10adac970a34.txt";
  const { session } = seedSession("synthetic-sdk-preflight", "key", [{
    name: "Write",
    inputSchema: { type: "object", properties: { path: { type: "string" }, content: { type: "string" } } },
  }]);
  session.resetSyntheticArtifactGuard("");

  const unary = await blockedSyntheticNativeExecIfNeeded(
    { session },
    "writeArgs",
    { toolCallId: "native-artifact", path: artifact, fileText: "internal" },
    false,
  );
  assert.ok(unary.__ccJson.error);
  assert.match(JSON.stringify(unary), /agent-tools artifact handoff is disabled/);
  assert.equal(session.currentRound, null);

  const stream = blockedSyntheticNativeExecIfNeeded(
    { session },
    "shellStreamArgs",
    { toolCallId: "native-artifact-readback", command: `head ${artifact}` },
    true,
  );
  const chunks = [];
  for await (const chunk of stream) chunks.push(chunk.__ccJson);
  assert.equal(chunks.at(-1).exit.code, 1);
  assert.equal(session.currentRound, null);
});

test("result-aware artifact guard blocks renamed copies and their readback, then resets on a real user turn", async () => {
  const largeMcpResult = `${"architecture-node: value\n".repeat(1800)}tail`;
  const renamedPath = "/tmp/cursor-result-cache.txt";
  const { session, output } = seedSession("synthetic-result-aware", "key", [
    { name: "Write", inputSchema: { type: "object", properties: { path: { type: "string" }, content: { type: "string" } } } },
    { name: "Read", inputSchema: { type: "object", properties: { path: { type: "string" } } } },
    { name: "Bash", inputSchema: { type: "object", properties: { command: { type: "string" } } } },
  ]);
  session.resetSyntheticArtifactGuard("summarize the repository");
  assert.equal(session.rememberLargeMcpResult(largeMcpResult, "codebase-memory-mcp"), true);

  const write = await session.openClientTool({
    source: "patched-unary",
    rawToolCallId: "renamed-write",
    name: "Write",
    input: { path: renamedPath, content: largeMcpResult },
    resultAdapter: (value) => value,
  });
  assert.equal(write.isError, true);
  assert.equal(write.structuredContent.reason, "copied_large_mcp_result");
  assert.equal(session.currentRound, null);
  assert.doesNotMatch(output.text(), /tool_call/);

  const read = await session.openClientTool({
    source: "patched-unary",
    rawToolCallId: "renamed-read",
    name: "Read",
    input: { path: renamedPath },
    resultAdapter: (value) => value,
  });
  assert.equal(read.structuredContent.reason, "rejected_artifact_followup");
  assert.match(read.content, /already been rejected 2 times/);

  const shell = await session.openClientTool({
    source: "patched-stream",
    rawToolCallId: "renamed-shell",
    name: "Bash",
    input: { command: `wc -c ${renamedPath}` },
    resultAdapter: (value) => value,
  });
  assert.equal(shell.structuredContent.reason, "rejected_artifact_followup");
  assert.equal(session.currentRound, null);

  session.resetSyntheticArtifactGuard("now do something unrelated");
  assert.equal(session.syntheticArtifactDecision("Read", { path: renamedPath }), null);
  assert.equal(session.syntheticArtifactDecision("Write", { path: "/tmp/other.txt", content: largeMcpResult }), null);
});

test("an explicitly user-requested artifact path is never swallowed by the internal guard", () => {
  const artifact = "agent-tools/367167f6-68b2-4941-84dd-e7e40affbb43.txt";
  const largeMcpResult = "x".repeat(40_000);
  const session = new Session("synthetic-user-override", "key");
  session.resetSyntheticArtifactGuard(`Write the exact MCP result to ${artifact}`);
  session.rememberLargeMcpResult(largeMcpResult, "Lookup");
  assert.equal(session.syntheticArtifactDecision("Write", { path: artifact, content: largeMcpResult }), null);
  assert.equal(session.syntheticArtifactDecision("Bash", { command: `head ${artifact}` }), null);
});

test("the neutral manifest never teaches internal artifact or wrapper behavior", () => {
  const external = toolManifest([{ name: "mcp__codebase-memory-mcp__get_architecture", description: "graph" }]);
  assert.doesNotMatch(external, /agent-tools|GetMcpTools|CallMcpTool|MCP server|MCP transport/i);
  const harnessOnly = toolManifest([{ name: "Write", description: "write a user-requested file" }]);
  assert.doesNotMatch(harnessOnly, /agent-tools/);
});

test("native result builders preserve failures and completeness metadata", () => {
  const failed = CC_CASES.readArgs.buildResult("permission denied", { path: "/x" }, true, { cwd: "" });
  assert.ok(failed.error);
  assert.equal(failed.success, undefined);
  const bounded = buildReadSuccess("one\ntwo", { path: "/x", limit: 2 });
  assert.equal(bounded.success.truncated, true);
  const structured = buildReadSuccess({ content: "one", truncated: false, rangeApplied: true, totalLines: 9, fileSize: 99 }, { path: "/x" });
  assert.equal(structured.success.totalLines, "9");
  const write = buildWriteSuccess({ fileContentAfterWrite: "actual", linesCreated: 1 }, { path: "/x", fileText: "requested", returnFileContentAfterWrite: true });
  assert.equal(write.success.fileContentAfterWrite, "actual");
  const ompStatus = "[/repo/a.py#A1B2]\nSuccessfully wrote 9 bytes to /repo/a.py";
  const statusWrite = buildWriteSuccess(ompStatus, { path: "/repo/a.py", fileText: "requested", returnFileContentAfterWrite: true });
  assert.equal(statusWrite.success.fileContentAfterWrite, "requested");
  assert.equal(statusWrite.success.fileSize, String(Buffer.byteLength("requested")));
});

test("MCP result builder preserves images, errors, and structured content", () => {
  const result = mcpDispatchResult("bad", true, [{ data: "QUJD", mimeType: "image/png" }], { code: "E" });
  assert.equal(result.success.isError, true);
  assert.deepEqual(result.success.structuredContent, { code: "E" });
  assert.ok(result.success.content.some((part) => part.image));
  const unavailable = typedUnavailableResult("grepArgs");
  assert.match(JSON.stringify(unavailable), /not available|unavailable/i);
});

test("inline MCP result images are opt-in because serialization does not prove model-route support", () => {
  const previous = process.env.CURSOR_COMPOSER_MCP_IMAGE_RESULTS;
  try {
    delete process.env.CURSOR_COMPOSER_MCP_IMAGE_RESULTS;
    assert.equal(mcpImageResultsEnabled(), false);
    process.env.CURSOR_COMPOSER_MCP_IMAGE_RESULTS = "1";
    assert.equal(mcpImageResultsEnabled(), true);
    process.env.CURSOR_COMPOSER_MCP_IMAGE_RESULTS = "true";
    assert.equal(mcpImageResultsEnabled(), true);
    process.env.CURSOR_COMPOSER_MCP_IMAGE_RESULTS = "0";
    assert.equal(mcpImageResultsEnabled(), false);
  } finally {
    if (previous === undefined) delete process.env.CURSOR_COMPOSER_MCP_IMAGE_RESULTS;
    else process.env.CURSOR_COMPOSER_MCP_IMAGE_RESULTS = previous;
  }
});

test("default image continuation bypasses the paused MCP callback and performs faithful recovery", async () => {
  const previous = process.env.CURSOR_COMPOSER_MCP_IMAGE_RESULTS;
  delete process.env.CURSOR_COMPOSER_MCP_IMAGE_RESULTS;
  const cursorKey = "image-recovery-key";
  const { session } = seedSession("image-result-recovery", cursorKey);
  let adapterCalls = 0;
  const opened = await openTool(session, {
    adapter(value) {
      adapterCalls++;
      return value;
    },
  });
  session.activeRes = null;
  session.responseWriter = null;

  const sent = [];
  const agent = {
    async send(message) {
      sent.push(message);
      return {
        async wait() { return { status: "finished", result: "IMAGE_RECOVERY_OK" }; },
        async cancel() {},
      };
    },
    async close() {},
  };
  platforms.set(keyHash(cursorKey), {
    promise: Promise.resolve({
      async resumeAgent() { throw new Error("agent not found"); },
      async createAgent() { return agent; },
      async getAgentMessages() { return []; },
    }),
    stateRoot: TEST_STATE_ROOT,
    lastUsed: Date.now(),
    fp: keyFingerprint(cursorKey),
  });

  try {
    const input = {
      type: "tool_results",
      results: [{
        toolCallId: opened.call.wireId,
        content: "generated image",
        images: [{ data: "QUJD", mimeType: "image/png" }],
      }],
    };
    opened.round.commitResults(input.results);
    const plan = toolResultRecoveryPlan(opened.round, input, session.seededSystem);
    assert.equal(plan.requiresFreshRecovery, true);
    assert.equal(plan.remainingUnreceipted, 0);
    assert.equal(plan.resultImages.length, 1);

    const response = new MockResponse();
    await handleContinue(request(), response, continuationBody(input.results), cursorKey);
    await waitFor(() => response.ended, "image recovery terminal");
    await assert.rejects(opened.promise, /faithful fresh send/);

    assert.equal(adapterCalls, 0, "the unreliable inline MCP image callback must not be invoked by default");
    assert.equal(sent.length, 1);
    assert.equal(typeof sent[0], "object");
    assert.deepEqual(sent[0].images, [{ data: "QUJD", mimeType: "image/png" }]);
    assert.match(sent[0].text, /generated image/);
    assert.match(sent[0].text, new RegExp(opened.call.wireId));
    assert.match(response.text(), /IMAGE_RECOVERY_OK/);
  } finally {
    platforms.delete(keyHash(cursorKey));
    if (previous === undefined) delete process.env.CURSOR_COMPOSER_MCP_IMAGE_RESULTS;
    else process.env.CURSOR_COMPOSER_MCP_IMAGE_RESULTS = previous;
  }
});

test("incremental parallel image results wait for siblings and recover from all durable receipts", async () => {
  const previous = process.env.CURSOR_COMPOSER_MCP_IMAGE_RESULTS;
  delete process.env.CURSOR_COMPOSER_MCP_IMAGE_RESULTS;
  const cursorKey = "parallel-image-recovery-key";
  const { session } = seedSession("parallel-image-result-recovery", cursorKey);
  const first = await openTool(session, { rawId: "parallel-image-first", awaiting: false });
  const secondPromise = session.openClientTool({
    source: "test",
    rawToolCallId: "parallel-image-second",
    name: "Lookup",
    input: { q: "second" },
    resultAdapter: (value) => value,
  });
  secondPromise.catch(() => {});
  await waitFor(() => first.round.fifo.length === 2, "parallel image call registration");
  session.flushJournaledCalls();
  const second = first.round.calls.get(first.round.fifo[1]);
  await waitFor(() => second.handedAt != null, "parallel image sibling handoff");
  first.round.markAwaitingResults();
  session.activeRes = null;
  session.responseWriter = null;

  const sent = [];
  const agent = {
    async send(message) {
      sent.push(message);
      return {
        async wait() { return { status: "finished", result: "PARALLEL_IMAGE_RECOVERY_OK" }; },
        async cancel() {},
      };
    },
    async close() {},
  };
  platforms.set(keyHash(cursorKey), {
    promise: Promise.resolve({
      async resumeAgent() { throw new Error("agent not found"); },
      async createAgent() { return agent; },
      async getAgentMessages() { return []; },
    }),
    stateRoot: TEST_STATE_ROOT,
    lastUsed: Date.now(),
    fp: keyFingerprint(cursorKey),
  });

  try {
    const partial = new MockResponse();
    await handleContinue(request(), partial, continuationBody([{
      toolCallId: first.call.wireId,
      content: "first image result",
      images: [{ data: "QUJD", mimeType: "image/png" }],
    }]), cursorKey);
    assert.match(partial.text(), /partial_results_deferred_for_fidelity/);
    assert.equal(first.round.pendingCount, 2);
    assert.equal(first.round.unreceiptedOwedCallCount, 1);
    assert.equal(sent.length, 0);

    const final = new MockResponse();
    await handleContinue(request(), final, continuationBody([{
      toolCallId: second.wireId,
      content: "second text result",
    }]), cursorKey);
    await waitFor(() => final.ended, "parallel image recovery terminal");
    await assert.rejects(first.promise, /faithful fresh send/);
    await assert.rejects(secondPromise, /faithful fresh send/);

    assert.equal(sent.length, 1);
    assert.deepEqual(sent[0].images, [{ data: "QUJD", mimeType: "image/png" }]);
    assert.match(sent[0].text, /first image result/);
    assert.match(sent[0].text, /second text result/);
    assert.match(final.text(), /PARALLEL_IMAGE_RECOVERY_OK/);
  } finally {
    platforms.delete(keyHash(cursorKey));
    if (previous === undefined) delete process.env.CURSOR_COMPOSER_MCP_IMAGE_RESULTS;
    else process.env.CURSOR_COMPOSER_MCP_IMAGE_RESULTS = previous;
  }
});

test("a concurrent final image result supersedes the old response and owns faithful recovery", async () => {
  const previous = process.env.CURSOR_COMPOSER_MCP_IMAGE_RESULTS;
  delete process.env.CURSOR_COMPOSER_MCP_IMAGE_RESULTS;
  const cursorKey = "concurrent-image-recovery-key";
  const { session, output } = seedSession("concurrent-image-result-recovery", cursorKey);
  const opened = await openTool(session);
  let oldRunCancels = 0;
  session.run = { async cancel() { oldRunCancels++; } };
  session.agent = { async close() {} };

  const sent = [];
  const agent = {
    async send(message) {
      sent.push(message);
      return {
        async wait() { return { status: "finished", result: "CONCURRENT_IMAGE_RECOVERY_OK" }; },
        async cancel() {},
      };
    },
    async close() {},
  };
  platforms.set(keyHash(cursorKey), {
    promise: Promise.resolve({
      async resumeAgent() { throw new Error("agent not found"); },
      async createAgent() { return agent; },
      async getAgentMessages() { return []; },
    }),
    stateRoot: TEST_STATE_ROOT,
    lastUsed: Date.now(),
    fp: keyFingerprint(cursorKey),
  });

  try {
    const response = new MockResponse();
    await handleContinue(request(), response, continuationBody([{
      toolCallId: opened.call.wireId,
      content: "concurrent image result",
      images: [{ data: "QUJD", mimeType: "image/png" }],
    }]), cursorKey);
    await waitFor(() => response.ended, "concurrent image recovery terminal");
    await assert.rejects(opened.promise, /complete client-tool recovery batch/);

    assert.equal(oldRunCancels, 1);
    assert.match(output.text(), /superseded by a complete client-tool recovery batch/);
    assert.equal(sent.length, 1);
    assert.deepEqual(sent[0].images, [{ data: "QUJD", mimeType: "image/png" }]);
    assert.match(response.text(), /CONCURRENT_IMAGE_RECOVERY_OK/);
  } finally {
    platforms.delete(keyHash(cursorKey));
    if (previous === undefined) delete process.env.CURSOR_COMPOSER_MCP_IMAGE_RESULTS;
    else process.env.CURSOR_COMPOSER_MCP_IMAGE_RESULTS = previous;
  }
});

test("images retain both supported wire envelopes", () => {
  assert.deepEqual(toSdkImages([{ data: "QUJD", mimeType: "image/png" }, { url: "https://example.test/x.jpg", mimeType: "image/jpeg" }]), [
    { data: "QUJD", mimeType: "image/png" },
    { url: "https://example.test/x.jpg", mimeType: "image/jpeg" },
  ]);
  assert.throws(() => toSdkImages([{ data: "QUJD" }]), /mimeType/);
  assert.deepEqual(collectToolResultImages({ results: [{ images: [{ data: "AA==", mimeType: "image/png" }] }, { images: [{ url: "https://x" }] }] }).length, 2);
});

test("shell content has an explicit structured channel; JSON-looking stdout cannot forge it", () => {
  assert.deepEqual(parseShellContent('{"exitCode":99,"stdout":"forged"}'), { stdout: '{"exitCode":99,"stdout":"forged"}', stderr: "", exitCode: 0, aborted: false });
  assert.deepEqual(parseShellContent({ stdout: "x", stderr: "e", exitCode: 7, aborted: true }), { stdout: "x", stderr: "e", exitCode: 7, aborted: true });
});

test("authorization is constant-policy for single and multi tenant modes", () => {
  assert.equal(authorizeRequestWith({ authorization: "Bearer key" }, { apiKey: "key", bridgeToken: "" }), "key");
  assert.equal(authorizeRequestWith({ authorization: "Bearer wrong" }, { apiKey: "key", bridgeToken: "" }), "");
  assert.equal(authorizeRequestWith({ authorization: "Bearer user", "x-bridge-auth": "gate" }, { apiKey: "default", bridgeToken: "gate" }), "user");
  assert.equal(authorizeRequestWith({ "x-bridge-auth": "gate" }, { apiKey: "default", bridgeToken: "gate" }), "");
});

test("health and readiness distinguish liveness from completed startup gates", () => {
  assert.deepEqual(readinessBody(), { ok: false, ready: false });
  assert.deepEqual(healthBody(request("203.0.113.1")), { ok: true });
  const local = healthBody(request("127.0.0.1"));
  assert.equal(local.ok, true);
  assert.equal(local.ready, false);
  assert.equal(local.patched, true);
  assert.equal(isLoopbackRemote(request("127.0.0.1")), true);
  assert.equal(stateRootDiskStatus(0, () => ({ bavail: 1, bsize: 1 })).ok, false);
  assert.equal(stateRootDiskStatus(0, () => ({ bavail: Number.MAX_SAFE_INTEGER, bsize: 1 })).ok, true);
});

test("disk-pressure admission rejects new turns before creating recoverability state", async (t) => {
  const moduleUrl = new URL("./cursor-agent-bridge.mjs", import.meta.url).href;
  const diskPressureRoot = mkdtempSync(path.join(tmpdir(), "cursor-disk-pressure-"));
  t.after(() => rmSync(diskPressureRoot, { recursive: true, force: true }));
  const code = `
    const bridge = await import(${JSON.stringify(moduleUrl)});
    const response = {
      status: 0,
      headers: {},
      body: "",
      writeHead(status, headers = {}) { this.status = status; this.headers = headers; },
      end(chunk = "") { this.body += String(chunk); },
    };
    await bridge.handleTurn(
      { headers: {}, socket: { remoteAddress: "127.0.0.1" } },
      response,
      { sessionId: "disk-pressure-new-turn", input: { type: "user", text: "must not start" } },
      "cursor-key",
    );
    const retryInput = {
      type: "user",
      text: "already completed under healthy capacity",
      clientMessageId: "ccm2_disk_pressure_replay",
    };
    const requestHash = bridge.completedTurnRequestHash(retryInput);
    bridge.writeCompletedTurnReceipt(
      "cursor-key", "disk-pressure-replay", retryInput.clientMessageId, requestHash,
      [{ type: "text", delta: "durable replay" }, { type: "turn_end", stop_reason: "end_turn" }],
      {},
      { requestKind: "fresh", identityPolicy: "legacy-client-message-v1" },
    );
    const replay = {
      status: 0, headers: {}, body: "",
      writeHead(status, headers = {}) { this.status = status; this.headers = headers; },
      write(chunk = "") { this.body += String(chunk); return true; },
      end(chunk = "") { this.body += String(chunk); },
    };
    await bridge.handleTurn(
      { headers: {}, socket: { remoteAddress: "127.0.0.1" } },
      replay,
      { sessionId: "disk-pressure-replay", input: retryInput },
      "cursor-key",
    );
    process.stdout.write(JSON.stringify({
      status: response.status,
      body: JSON.parse(response.body),
      sessions: bridge.sessions.size,
      replayStatus: replay.status,
      replayBody: replay.body,
    }));
  `;
  const child = spawn(process.execPath, ["--input-type=module", "--eval", code], {
    cwd: path.dirname(new URL(import.meta.url).pathname),
    env: {
      ...process.env,
      CURSOR_AGENT_STATE_ROOT: diskPressureRoot,
      CURSOR_COMPOSER_STATE_ROOT_MIN_FREE_BYTES: String(Number.MAX_SAFE_INTEGER),
    },
    stdio: ["ignore", "pipe", "pipe"],
  });
  let stdout = "";
  let stderr = "";
  child.stdout.on("data", (chunk) => { stdout += chunk; });
  child.stderr.on("data", (chunk) => { stderr += chunk; });
  const exit = await new Promise((resolve, reject) => {
    child.once("error", reject);
    child.once("exit", (exitCode, signal) => resolve({ code: exitCode, signal }));
  });
  assert.deepEqual(exit, { code: 0, signal: null }, stderr);
  const result = JSON.parse(stdout);
  assert.equal(result.status, 507);
  assert.equal(result.body.error.code, "durable_state_capacity");
  assert.equal(result.sessions, 0);
  assert.equal(result.replayStatus, 200);
  assert.match(result.replayBody, /completed_turn_replay/);
  assert.match(result.replayBody, /durable replay/);
});

test("request-shape rejections do not consume shared unresolved receipt quota", async () => {
  const ledger = path.join(TEST_STATE_ROOT, ".cct-completed-turns", ".unresolved-reservations.json");
  const count = () => {
    try { return Object.keys(JSON.parse(readFileSync(ledger, "utf8")).entries || {}).length; }
    catch { return 0; }
  };
  const before = count();
  for (let i = 0; i < 8; i++) {
    const response = new MockResponse();
    await handleTurn(
      request(),
      response,
      neutralBody({
        sessionId: `rejected-quota-${i}`,
        input: {
          type: "user",
          text: "must fail before reservation",
          clientMessageId: `ccm2_rejected_quota_${i}`,
          legacyRecoveryKey: "invalid",
        },
      }),
      "cursor-key",
    );
    assert.equal(response.status, 422);
  }
  assert.equal(count(), before);
});

test("durable maintenance sweeps aged unclaimed candidates without a canonical record", () => {
  const dir = path.join(TEST_STATE_ROOT, ".cct-completed-turns");
  mkdirSync(dir, { recursive: true });
  const orphan = path.join(dir, `${"a".repeat(64)}.json.cas.r1.crashed.candidate`);
  writeFileSync(orphan, "orphan\n");
  const old = new Date(0);
  utimesSync(orphan, old, old);
  runDurableMaintenance();
  assert.equal(existsSync(orphan), false);
});

test("the SDK MCP HTTP route is loopback-only and path decoding fails closed", () => {
  const local = request("127.0.0.1");
  local.url = "/mcp/session%20one/server?ignored=1";
  assert.deepEqual(classifyMcpRoute(local), { sessionId: "session one", serverKey: "server" });

  const forwarded = request("127.0.0.1");
  forwarded.url = "/mcp/session";
  forwarded.headers["x-forwarded-for"] = "203.0.113.7";
  assert.deepEqual(classifyMcpRoute(forwarded), { error: "the SDK MCP shim is loopback-only", status: 403 });

  const malformed = request("127.0.0.1");
  malformed.url = "/mcp/%E0%A4%A";
  assert.deepEqual(classifyMcpRoute(malformed), { error: "malformed MCP path encoding", status: 400 });

  const extra = request("127.0.0.1");
  extra.url = "/mcp/session/server/extra";
  assert.deepEqual(classifyMcpRoute(extra), { error: "malformed MCP path", status: 400 });
});

test("non-loopback bind requires an authenticated or explicit insecure deployment", () => {
  assert.equal(bindHostIsLoopback("127.0.0.1"), true);
  assert.equal(bindHostIsLoopback("0.0.0.0"), false);
  assert.equal(validateBindHost("0.0.0.0", false, false).ok, false);
  assert.equal(validateBindHost("0.0.0.0", true, false).ok, true);
  assert.match(validateBindHost("0.0.0.0", false, true).warn, /plaintext/);
});

test("utility helpers preserve strict types and bounded output", () => {
  assert.deepEqual(wrapToolInput("raw"), { input: "raw" });
  assert.deepEqual(wrapToolInput({ q: "x" }), { q: "x" });
  assert.match(truncateLiveToolResult("abcdefgh", 4), /abcd[\s\S]*truncated by proxy/);
  assert.equal(isConversationTooLong("ERROR_CONVERSATION_TOO_LONG"), true);
  assert.equal(keyFingerprint("a").length, 64);
  const name = "TEST_CCT_ENV_INT";
  process.env[name] = "17";
  assert.equal(envInt(name, 3, { min: 1 }), 17);
  process.env[name] = "10m";
  assert.equal(envInt(name, 3, { min: 1 }), 3);
  delete process.env[name];
});
