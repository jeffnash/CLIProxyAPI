import assert from "node:assert/strict";
import { mkdtempSync, mkdirSync, readFileSync, rmSync, utimesSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import test from "node:test";
import {
  CallState,
  DeferredInputState,
  RoundJournal,
  RoundState,
  RoutingTokenCodec,
  TerminalReason,
  ToolRound,
  ToolRoundError,
  canonicalJSONString,
  createRoundInfrastructure,
  createTestRound,
  hashClientToolResult,
  hashLegacyClientToolResult,
  loadOrCreateRoutingKey,
  MAX_DEFERRED_INPUT_RECORDS,
  probeStateRoot,
  terminalizeOrphanedRounds,
} from "./tool-round.mjs";

function withTempDir(t) {
  const dir = mkdtempSync(path.join(tmpdir(), "cct-round-"));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  return dir;
}

function callbackLog(log, id) {
  return {
    resolve(result) { log.push(["resolve", id, result]); },
    reject(error) { log.push(["reject", id, error.message]); },
  };
}

function openHanded(round, log, rawId, name = "Read", input = { path: "/x" }) {
  const call = round.openCall({ source: "test", rawToolCallId: rawId, name, input, callback: callbackLog(log, rawId) });
  round.markHanded(call.wireId);
  return call;
}

test("canonical JSON sorts object keys recursively and preserves array order", () => {
  assert.equal(canonicalJSONString({ z: 1, a: { y: 2, x: 3 }, list: [{ b: 2, a: 1 }, 4] }),
    '{"a":{"x":3,"y":2},"list":[{"a":1,"b":2},4],"z":1}');
  assert.equal(
    hashClientToolResult({ content: { b: 2, a: 1 }, isError: false }),
    hashClientToolResult({ isError: false, content: { a: 1, b: 2 } }),
  );
  assert.notEqual(
    hashClientToolResult({ content: [1, 2], isError: false }),
    hashClientToolResult({ content: [2, 1], isError: false }),
  );
});

test("canonical JSON preserves prototype-named own keys and hashes them distinctly", () => {
  const first = JSON.parse(`{"content":{"__proto__":"one","constructor":"first"}}`);
  const second = JSON.parse(`{"content":{"__proto__":"two","constructor":"first"}}`);
  assert.equal(canonicalJSONString(first), `{"content":{"__proto__":"one","constructor":"first"}}`);
  assert.notEqual(hashClientToolResult(first), hashClientToolResult(second));
});

test("canonical result hash includes error, structured content, and images", () => {
  const base = { content: "same", isError: false, images: [], structuredContent: null };
  assert.notEqual(hashClientToolResult(base), hashClientToolResult({ ...base, isError: true }));
  assert.notEqual(hashClientToolResult(base), hashClientToolResult({ ...base, structuredContent: { ok: true } }));
  assert.notEqual(hashClientToolResult(base), hashClientToolResult({ ...base, images: [{ data: "AA==", mimeType: "image/png" }] }));
  assert.throws(() => hashClientToolResult({ content: { nope: undefined } }), (error) => error.code === "non_json_result");
});

test("semantic V2 result identity is transport-neutral but order-sensitive", () => {
  assert.equal(
    hashClientToolResult({ content: [{ type: "text", text: "first" }, { type: "text", text: "second" }] }),
    hashClientToolResult({ content: "first\n\nsecond" }),
  );
  assert.equal(
    hashClientToolResult({ content: "failed", is_error: true }),
    hashClientToolResult({ contentBlocks: [{ type: "text", text: "failed" }], isError: true }),
  );
  assert.equal(
    hashClientToolResult({ content: "image", images: [{ data: "QUJD", mimeType: "image/png" }] }),
    hashClientToolResult({ contentBlocks: [
      { type: "text", text: "image" },
      { type: "image_url", image_url: { url: "data:image/png;base64,QUJD" } },
    ] }),
  );
  assert.equal(
    hashClientToolResult({ content: "image", images: [{ data: "Q Q\n==", mimeType: "IMAGE/PNG; charset=binary" }] }),
    hashClientToolResult({ content: "image", images: [{ data: "QQ==", mimeType: "image/png" }] }),
  );
  assert.notEqual(
    hashClientToolResult({ contentBlocks: [{ type: "text", text: "a" }, { data: "QUJD", mimeType: "image/png" }] }),
    hashClientToolResult({ contentBlocks: [{ data: "QUJD", mimeType: "image/png" }, { type: "text", text: "a" }] }),
  );
  assert.notEqual(
    hashClientToolResult({ content: "ok" }),
    hashClientToolResult({ content: "ok", structuredContent: null }),
  );
  assert.notEqual(
    hashClientToolResult({ contentBlocks: [{ type: "image_url", image_url: { url: "https://example.test/a.png", detail: "low" } }] }),
    hashClientToolResult({ contentBlocks: [{ type: "image_url", image_url: { url: "https://example.test/a.png", detail: "high" } }] }),
  );
  assert.throws(
    () => hashClientToolResult({ content: "bad", isError: true, is_error: false }),
    (error) => error.code === "invalid_tool_result",
  );
  assert.throws(
    () => hashClientToolResult({ contentBlocks: [{ type: "image", data: "missing-mime" }] }),
    (error) => error.code === "invalid_tool_result",
  );
  assert.throws(
    () => hashClientToolResult({ contentBlocks: [{ type: "image", data: "***", mimeType: "image/png" }] }),
    (error) => error.code === "invalid_tool_result",
  );
});

test("signed routing tokens round-trip and reject tampering", () => {
  const codec = new RoutingTokenCodec(Buffer.alloc(32, 1));
  const route = codec.newRoute();
  const token = codec.issue(route, 35);
  assert.deepEqual(codec.parse(token), { route, ordinal: 35, token });
  const tampered = token.slice(0, -1) + (token.endsWith("A") ? "B" : "A");
  assert.throws(() => codec.parse(tampered), (error) => error.code === "invalid_tool_call_id");
  assert.throws(() => new RoutingTokenCodec(Buffer.alloc(8)), (error) => error.code === "routing_key_invalid");
  assert.throws(() => codec.parse(token.replace(/^cct1_/, "cct2_")), (error) => error.code === "invalid_tool_call_id");
});

test("routing key creation is stable and exactly 32 bytes", (t) => {
  const dir = withTempDir(t);
  const first = loadOrCreateRoutingKey(dir);
  const second = loadOrCreateRoutingKey(dir);
  assert.equal(first.length, 32);
  assert.deepEqual(first, second);
  assert.deepEqual(readFileSync(path.join(dir, ".client-tool-routing.key")), first);
});

test("an existing malformed routing key is refused and never replaced", (t) => {
  const dir = withTempDir(t);
  const keyPath = path.join(dir, ".client-tool-routing.key");
  writeFileSync(keyPath, "truncated", { mode: 0o600 });
  assert.throws(() => loadOrCreateRoutingKey(dir), (error) => error.code === "routing_key_invalid");
  assert.equal(readFileSync(keyPath, "utf8"), "truncated");
});

test("independent bridge infrastructures share one atomically published routing key", (t) => {
  const dir = withTempDir(t);
  const first = createRoundInfrastructure(dir);
  const second = createRoundInfrastructure(dir);
  const route = first.codec.newRoute();
  const token = first.codec.issue(route, 3);
  assert.deepEqual(second.codec.parse(token), { route, ordinal: 3, token });
  assert.equal(readFileSync(path.join(dir, ".client-tool-routing.key")).length, 32);
});

test("journal creates, revises, and detects stale revisions", (t) => {
  const dir = withTempDir(t);
  const { journal, codec } = createRoundInfrastructure(dir, { secret: Buffer.alloc(32, 2) });
  const round = new ToolRound({
    sessionId: "sess",
    runEpoch: 1,
    roundSeq: 1,
    clientLeaseToken: "18446744073709551615",
    journal,
    codec,
  });
  assert.equal(round.revision, 1);
  const saved = journal.read(round.route);
  assert.equal(saved.state, RoundState.COLLECTING);
  assert.equal(saved.revision, 1);
  assert.equal(saved.clientLeaseToken, "18446744073709551615", "uint64 lease tokens remain lossless in the journal");
  assert.equal(ToolRound.load(journal, codec, round.route).clientLeaseToken, "18446744073709551615");
  assert.throws(() => journal.save(saved, 0), (error) => error.code === "journal_revision_conflict");
  const call = round.openCall({ name: "Read", input: { path: "/x" }, callback: callbackLog([], "a") });
  assert.equal(round.revision, 2);
  assert.equal(journal.read(round.route).calls[0].wireId, call.wireId);
});

test("crash-left temporary snapshots never replace the last committed revision", (t) => {
  const dir = withTempDir(t);
  const { journal, codec } = createRoundInfrastructure(dir, { secret: Buffer.alloc(32, 15) });
  const round = new ToolRound({ sessionId: "sess", journal, codec });
  const committed = readFileSync(journal.file(round.route), "utf8");
  writeFileSync(path.join(journal.dir, `.${round.route}.json.crash.tmp`), "{not-json", { mode: 0o600 });
  assert.equal(readFileSync(journal.file(round.route), "utf8"), committed);
  assert.equal(journal.read(round.route).revision, 1);
  assert.equal(journal.records().length, 1, "temporary files are never enumerated as journals");
});

test("another bridge process can receipt a shared round and fences a stale writer", (t) => {
  const dir = withTempDir(t);
  const first = createRoundInfrastructure(dir);
  const second = createRoundInfrastructure(dir);
  const original = new ToolRound({ sessionId: "sess", journal: first.journal, codec: first.codec });
  const call = openHanded(original, [], "raw");
  original.markAwaitingResults();

  const landedElsewhere = ToolRound.load(second.journal, second.codec, original.route);
  const committed = landedElsewhere.commitResults([{ toolCallId: call.wireId, content: "from another bridge" }]);
  assert.deepEqual(committed.additions, [call.wireId]);
  assert.equal(second.journal.read(original.route).calls[0].receipt.result.content, "from another bridge");

  assert.throws(
    () => original.terminalize(TerminalReason.PENDING_TIMEOUT, "stale process watchdog"),
    (error) => error.code === "journal_revision_conflict",
    "a stale callback owner must never overwrite the durable receipt",
  );
  assert.equal(second.journal.read(original.route).calls[0].receipt.result.content, "from another bridge");
});

test("journal rejects active locks and recovers demonstrably stale locks", (t) => {
  const dir = withTempDir(t);
  let now = 100_000;
  const journal = new RoundJournal(dir, { now: () => now, staleLockMs: 1_000 });
  const codec = new RoutingTokenCodec(Buffer.alloc(32, 3));
  const route = codec.newRoute();
  const lock = journal.lockDir(route);
  mkdirSync(lock);
  assert.throws(() => journal.acquire(route), (error) => error.code === "round_busy");
  const old = new Date(0);
  utimesSync(lock, old, old);
  now = 200_000;
  const release = journal.acquire(route);
  release();
});

test("ToolRound persists registration before exposing a call", (t) => {
  const dir = withTempDir(t);
  const { journal, codec } = createRoundInfrastructure(dir, { secret: Buffer.alloc(32, 4) });
  const round = new ToolRound({ sessionId: "sess", journal, codec });
  const call = round.openCall({ rawToolCallId: "sdk-a", name: "Bash", input: { command: "pwd" }, callback: callbackLog([], "a") });
  const saved = journal.read(round.route);
  assert.equal(saved.calls.length, 1);
  assert.equal(saved.calls[0].wireId, call.wireId);
  assert.equal(codec.parse(call.wireId).route, round.route);
  assert.equal(saved.calls[0].rawIdHash.length, 64);
  assert.equal(saved.calls[0].state, CallState.REGISTERED);
});

test("raw SDK id may refine registered arguments but freezes at transport handoff", () => {
  const round = createTestRound();
  const first = round.openCall({ rawToolCallId: "raw", name: "Read", input: { path: "/a" } });
  const again = round.openCall({ rawToolCallId: "raw", name: "Read", input: { path: "/a" } });
  assert.equal(again.wireId, first.wireId);
  assert.equal(round.calls.size, 1);
  const refined = round.openCall({ rawToolCallId: "raw", name: "Read", input: { path: "/a", limit: 20 } });
  assert.equal(refined.wireId, first.wireId);
  assert.deepEqual(refined.input, { limit: 20, path: "/a" });
  assert.throws(
    () => round.openCall({ rawToolCallId: "raw", name: "Write", input: { path: "/a" } }),
    (error) => error.code === "raw_tool_id_conflict",
  );
  round.markHanded(first.wireId);
  assert.throws(
    () => round.openCall({ rawToolCallId: "raw", name: "Read", input: { path: "/different" } }),
    (error) => error.code === "raw_tool_id_conflict",
  );
});

test("raw SDK id reuse compares the same JSON transport value and never strands a late waiter", () => {
  const log = [];
  const round = createTestRound();
  const a = round.openCall({
    rawToolCallId: "raw-a",
    name: "Read",
    input: { path: "/a", offset: undefined, marker: -0 },
    callback: callbackLog(log, "a-primary"),
  });
  const b = openHanded(round, log, "raw-b");
  round.markHanded(a.wireId);
  round.markAwaitingResults();
  round.applyResults([{ toolCallId: a.wireId, content: "A" }]);

  const duplicate = round.openCall({
    rawToolCallId: "raw-a",
    name: "Read",
    input: { marker: -0, offset: undefined, path: "/a" },
    callback: callbackLog(log, "a-late"),
  });
  assert.equal(duplicate.wireId, a.wireId);
  assert.equal(duplicate.newlyRegistered, false);
  assert.deepEqual(duplicate.input, { marker: 0, path: "/a" });
  assert.equal(round.outbound.includes(a.wireId), false, "a receipted duplicate must not be emitted again");
  assert.deepEqual(log.filter((entry) => entry[0] === "resolve").map((entry) => entry[1]), ["a-primary", "a-late"]);

  round.applyResults([{ toolCallId: b.wireId, content: "B" }]);
  assert.equal(round.terminal.reason, TerminalReason.COMPLETED);
});

test("parallel callbacks sharing one raw SDK id all resolve from one durable receipt", () => {
  const log = [];
  const round = createTestRound();
  const first = round.openCall({
    rawToolCallId: "shared",
    name: "Read",
    input: { path: "/a" },
    callback: callbackLog(log, "first"),
  });
  const second = round.openCall({
    rawToolCallId: "shared",
    name: "Read",
    input: { path: "/a" },
    callback: callbackLog(log, "second"),
  });
  assert.equal(second.wireId, first.wireId);
  assert.equal(round.calls.size, 1);
  round.markHanded(first.wireId);
  round.markAwaitingResults();
  round.applyResults([{ toolCallId: first.wireId, content: "once" }]);
  assert.deepEqual(log.filter((entry) => entry[0] === "resolve").map((entry) => entry[1]), ["first", "second"]);
});

test("full-batch conflict preflight mutates no call", () => {
  const log = [];
  const round = createTestRound();
  const a = openHanded(round, log, "a");
  const b = openHanded(round, log, "b");
  round.markAwaitingResults();
  assert.throws(
    () => round.applyResults([
      { toolCallId: a.wireId, content: "accepted only if all validate" },
      { toolCallId: b.wireId, content: "one" },
      { toolCallId: b.wireId, content: "two" },
    ]),
    (error) => error.code === "result_conflict",
  );
  assert.equal(a.resultHash, null);
  assert.equal(b.resultHash, null);
  assert.deepEqual(log, []);
});

test("results are durably receipted before callbacks run", (t) => {
  const dir = withTempDir(t);
  const { journal, codec } = createRoundInfrastructure(dir, { secret: Buffer.alloc(32, 5) });
  const observed = [];
  const round = new ToolRound({ sessionId: "sess", journal, codec });
  const call = round.openCall({
    rawToolCallId: "a",
    name: "Read",
    input: { path: "/x" },
    callback: {
      resolve() { observed.push(journal.read(round.route).calls[0].state); },
      reject(error) { throw error; },
    },
  });
  round.markHanded(call.wireId);
  round.markAwaitingResults();
  round.applyResults([{ toolCallId: call.wireId, content: "ok" }]);
  assert.deepEqual(observed, [CallState.RESULT_RECEIVED]);
  const saved = journal.read(round.route);
  assert.equal(saved.terminal.reason, TerminalReason.COMPLETED);
  assert.equal(Object.hasOwn(saved.calls[0].receipt, "semanticResult"), false,
    "new receipts must not persist a second full copy of large semantic content");
});

test("partial parallel batches leave only unanswered callbacks open", () => {
  const log = [];
  const round = createTestRound();
  const a = openHanded(round, log, "a");
  const b = openHanded(round, log, "b");
  round.markAwaitingResults();
  const first = round.applyResults([{ toolCallId: a.wireId, content: "A", isError: false }]);
  assert.deepEqual(first.additions, [a.wireId]);
  assert.equal(round.state, RoundState.AWAITING_RESULTS);
  assert.equal(round.pendingCount, 1);
  assert.equal(round.calls.get(a.wireId).state, CallState.CALLBACK_APPLIED);
  assert.equal(round.calls.get(b.wireId).state, CallState.HANDED_TO_TRANSPORT);
  round.applyResults([{ toolCallId: b.wireId, content: "B", isError: true }]);
  assert.equal(round.state, RoundState.TERMINAL);
  assert.equal(round.terminal.reason, TerminalReason.COMPLETED);
  assert.equal(round.pendingCount, 0);
  assert.deepEqual(log.map((entry) => entry.slice(0, 2)), [["resolve", "a"], ["resolve", "b"]]);
});

test("identical duplicate is idempotent and conflicting duplicate is rejected", () => {
  const log = [];
  const round = createTestRound();
  const call = openHanded(round, log, "a");
  round.markAwaitingResults();
  const result = { toolCallId: call.wireId, content: { z: 1, a: 2 }, isError: false };
  round.applyResults([result]);
  const duplicate = round.applyResults([{ toolCallId: call.wireId, content: { a: 2, z: 1 }, isError: false }]);
  assert.deepEqual(duplicate.additions, []);
  assert.deepEqual(duplicate.duplicates, [call.wireId]);
  assert.equal(log.filter((entry) => entry[0] === "resolve").length, 1);
  assert.throws(
    () => round.applyResults([{ toolCallId: call.wireId, content: "different", isError: false }]),
    (error) => error.code === "result_conflict" && error.status === 409,
  );
});

test("legacy V1 receipts migrate lazily to semantic V2 identity without rewriting history", (t) => {
  const dir = withTempDir(t);
  const { journal, codec } = createRoundInfrastructure(dir, { secret: Buffer.alloc(32, 21) });
  const round = new ToolRound({ sessionId: "legacy", journal, codec });
  const call = openHanded(round, [], "legacy-call");
  round.markAwaitingResults();

  const legacyResult = { content: "first\n\nsecond", images: [], isError: false, structuredContent: null };
  const legacyHash = hashLegacyClientToolResult(legacyResult);
  const record = journal.read(round.route);
  record.state = RoundState.APPLYING_RESULTS;
  record.calls[0].state = CallState.RESULT_RECEIVED;
  record.calls[0].resultHash = legacyHash;
  record.calls[0].resultHashVersion = null;
  record.calls[0].semanticResultHash = null;
  record.calls[0].receipt = { acceptedAt: 10, result: legacyResult };
  journal.save(record, round.revision);

  const recovered = ToolRound.load(journal, codec, round.route);
  const committed = recovered.commitResults([{
    toolCallId: call.wireId,
    contentBlocks: [{ type: "text", text: "first" }, { type: "text", text: "second" }],
    is_error: false,
  }]);
  assert.deepEqual(committed.additions, []);
  assert.deepEqual(committed.duplicates, [call.wireId]);
  const saved = journal.read(round.route).calls[0];
  assert.equal(saved.resultHash, legacyHash, "the immutable V1 receipt hash is retained");
  assert.equal(saved.resultHashVersion, 1);
  assert.equal(saved.semanticResultHash, hashClientToolResult({ content: "first\n\nsecond" }));
  assert.deepEqual(saved.receipt, { acceptedAt: 10, result: legacyResult });
});

test("deferred user input is journaled independently and advances exactly once", (t) => {
  const dir = withTempDir(t);
  const { journal, codec } = createRoundInfrastructure(dir, { secret: Buffer.alloc(32, 22) });
  const round = new ToolRound({ sessionId: "deferred", runEpoch: 7, roundSeq: 3, journal, codec });
  const input = {
    userText: "continue with the corrected result",
    images: [{ data: "QUJD", mimeType: "image/png" }],
    history: "bounded history",
    interruptRequested: true,
  };
  assert.deepEqual(round.queueDeferredInput("ccm1_message", input), {
    clientMessageId: "ccm1_message", duplicate: false, status: "queued",
  });
  assert.deepEqual(round.queueDeferredInput("ccm1_message", {
    ...input,
    system: "refreshed system prompt",
    history: "compacted history",
    historyFingerprint: "new-fingerprint",
    interruptRequested: false,
  }), {
    clientMessageId: "ccm1_message", duplicate: true, status: "queued",
  }, "mutable recovery context must not conflict with the same immutable user input");
  const rekeyed = round.queueDeferredInput("ccm1_message", { ...input, userText: "different" });
  assert.equal(rekeyed.rekeyed, true);
  assert.equal(rekeyed.requestedClientMessageId, "ccm1_message");
  assert.equal(rekeyed.status, "rekeyed_queued");
  assert.notEqual(rekeyed.clientMessageId, "ccm1_message");
  assert.deepEqual(
    round.queueDeferredInput("ccm1_message", { ...input, userText: "different" }),
    { ...rekeyed, duplicate: true, status: "rekeyed_duplicate" },
    "a retry of the conflicting intent must resolve to the same deterministic durable id",
  );

  let recovered = ToolRound.load(journal, codec, round.route);
  assert.equal(recovered.deferredInput("ccm1_message").state, DeferredInputState.SUPERSEDED);
  assert.equal(recovered.deferredInput(rekeyed.clientMessageId).state, DeferredInputState.QUEUED);
  recovered.markDeferredInputState(rekeyed.clientMessageId, DeferredInputState.DELIVERING);
  recovered = ToolRound.load(journal, codec, round.route);
  assert.equal(recovered.deferredInput(rekeyed.clientMessageId).state, DeferredInputState.DELIVERING);
  recovered.markDeferredInputState(rekeyed.clientMessageId, DeferredInputState.DELIVERED);
  assert.equal(ToolRound.load(journal, codec, round.route).deferredInput(rekeyed.clientMessageId).state, DeferredInputState.DELIVERED);
  assert.throws(
    () => recovered.markDeferredInputState(rekeyed.clientMessageId, DeferredInputState.QUEUED),
    (error) => error.code === "client_message_state_conflict",
  );
  assert.deepEqual(recovered.toolEpochState(), {
    route: recovered.route,
    runEpoch: 7,
    roundSeq: 3,
    roundState: RoundState.COLLECTING,
    pending: [],
    receipted: [],
    applied: [],
    terminalReason: null,
  });
});

test("uncertain deferred delivery preserves its exact SDK envelope until final response receipt", (t) => {
  const dir = withTempDir(t);
  const { journal, codec } = createRoundInfrastructure(dir, { secret: Buffer.alloc(32, 31) });
  const round = new ToolRound({ sessionId: "exact-deferred", journal, codec });
  const originalMessage = { text: "original SDK message", images: [{ data: "QUJD", dimension: { width: 1, height: 1 } }] };
  const originalAdvertise = [{
    name: "OriginalTool",
    description: "the descriptor used by the accepted SDK send",
    inputSchema: { type: "object", properties: { path: { type: "string" } }, required: ["path"] },
  }];
  round.queueDeferredInput("message-exact", {
    userText: "original intent",
    history: "original recovery context",
  });
  round.markDeferredInputState("message-exact", DeferredInputState.DELIVERING, {
    agentId: "agent-original",
    idempotencyKey: "send-key-original",
    message: originalMessage,
    advertise: originalAdvertise,
    inventoryEpoch: "cti1_original",
    model: "cursor-grok-4.5-xhigh",
    toolChoice: "auto",
    seededSystem: "original system",
  });

  // A retry may carry fresher mutable context, but it must not rewrite the
  // payload already bound to the durable SDK idempotency key.
  assert.deepEqual(round.queueDeferredInput("message-exact", {
    userText: "original intent",
    history: "newer retry context",
    system: "newer retry system",
  }), {
    clientMessageId: "message-exact",
    duplicate: true,
    status: "delivering",
  });
  let saved = ToolRound.load(journal, codec, round.route).deferredInput("message-exact");
  assert.deepEqual(saved.deliveryMessage, originalMessage);
  assert.deepEqual(saved.deliveryAdvertise, originalAdvertise);
  assert.equal(saved.deliveryAgentId, "agent-original");
  assert.equal(saved.deliveryIdempotencyKey, "send-key-original");

  // agent.send resolving is acceptance evidence, not proof that the resumed
  // model turn produced and durably receipted its final answer.
  round.markDeferredInputState("message-exact", DeferredInputState.DELIVERED, {
    evidence: "agent_send_resolved",
  });
  saved = ToolRound.load(journal, codec, round.route).deferredInput("message-exact");
  assert.deepEqual(saved.deliveryMessage, originalMessage);
  assert.deepEqual(saved.deliveryAdvertise, originalAdvertise);
  assert.equal(saved.deliveryFinalizedAt, undefined);

  round.requeueUncertainDeferredInput("message-exact");
  saved = ToolRound.load(journal, codec, round.route).deferredInput("message-exact");
  assert.equal(saved.state, DeferredInputState.QUEUED);
  assert.deepEqual(saved.deliveryMessage, originalMessage);
  assert.deepEqual(saved.deliveryAdvertise, originalAdvertise);

  round.markDeferredInputState("message-exact", DeferredInputState.DELIVERING);
  round.markDeferredInputState("message-exact", DeferredInputState.DELIVERED, {
    evidence: "completed_turn_receipt",
    finalized: true,
  });
  saved = ToolRound.load(journal, codec, round.route).deferredInput("message-exact");
  assert.equal(saved.state, DeferredInputState.DELIVERED);
  assert.ok(saved.deliveryFinalizedAt);
  assert.equal(saved.deliveryMessage, undefined);
  assert.equal(saved.deliveryAdvertise, undefined);
  assert.throws(
    () => round.requeueUncertainDeferredInput("message-exact"),
    (error) => error.code === "client_message_already_finalized",
  );
});

test("deferred snapshots keep one recovery context, supersede stale intent, and stay bounded", (t) => {
  const dir = withTempDir(t);
  const { journal, codec } = createRoundInfrastructure(dir, { secret: Buffer.alloc(32, 23) });
  const round = new ToolRound({ sessionId: "bounded-deferred", journal, codec });
  const history = "history:" + "x".repeat(128 * 1024);
  round.queueDeferredInput("message-0", { userText: "intent 0", history });
  round.markDeferredInputState("message-0", DeferredInputState.DELIVERING, {
    agentId: "agent-0",
    textHash: "a".repeat(64),
    hasImages: false,
    idempotencyKey: "send-key-0",
    message: "large exact send:" + "y".repeat(128 * 1024),
    advertise: [{ name: "OriginalTool", inputSchema: { type: "object" } }],
  });
  round.queueDeferredInput("message-1", { userText: "intent 1", history: "latest context" });

  const first = round.deferredInput("message-0");
  assert.equal(first.state, DeferredInputState.SUPERSEDED);
  assert.equal(first.supersedeReason, "delivery_uncertain_superseded");
  assert.equal(first.input, null);
  assert.equal(first.deliveryMessage, undefined);
  assert.equal(first.deliveryAdvertise, undefined);
  // Definitive late SDK evidence may still upgrade an uncertain tombstone.
  round.markDeferredInputState("message-0", DeferredInputState.DELIVERED, {
    evidence: "user_message_appended",
  });

  // A delayed replay of an old delivered id cannot roll shared recovery
  // context back to the large historical snapshot.
  round.queueDeferredInput("message-0", { userText: "intent 0", history });
  assert.equal(round.recoveryContext.history, "latest context");

  for (let index = 2; index < 100; index++) {
    round.queueDeferredInput(`message-${index}`, {
      userText: `intent ${index}`,
      history: `context ${index}`,
    });
  }
  const saved = journal.read(round.route);
  const queued = saved.deferredInputs.filter((entry) => entry.state === DeferredInputState.QUEUED);
  assert.equal(queued.length, 1);
  assert.equal(queued[0].clientMessageId, "message-99");
  assert.ok(saved.deferredInputs.length <= MAX_DEFERRED_INPUT_RECORDS + 1);
  assert.equal(saved.recoveryContext.history, "context 99");
  const serialized = JSON.stringify(saved);
  assert.equal(serialized.includes(history), false, "superseded history must not remain duplicated in tombstones");
  assert.equal(serialized.includes("large exact send"), false, "superseded SDK envelopes must not bloat tombstones");
});

test("terminal cleanup retains a round while deferred input is still owed", (t) => {
  const dir = withTempDir(t);
  const { journal, codec } = createRoundInfrastructure(dir, { secret: Buffer.alloc(32, 24), now: () => 10_000 });
  const round = new ToolRound({ sessionId: "owed-deferred", journal, codec });
  round.queueDeferredInput("owed-message", { userText: "do not delete me" });
  round.terminalize(TerminalReason.COMPLETED);
  assert.deepEqual(journal.cleanupTerminal({ ttlMs: 0, maxTerminal: 0 }), { removed: 0, retained: 0 });
  assert.ok(journal.read(round.route));
  round.markDeferredInputState("owed-message", DeferredInputState.SUPERSEDED, { reason: "test_cleanup" });
  assert.deepEqual(journal.cleanupTerminal({ ttlMs: 0, maxTerminal: 0 }), { removed: 1, retained: 0 });
  assert.equal(journal.read(round.route), null);
});

test("mixed signed routes are rejected before any receipt", () => {
  const log = [];
  const first = createTestRound({ secret: Buffer.alloc(32, 9) });
  const second = createTestRound({ secret: Buffer.alloc(32, 9), sessionId: "other" });
  const a = openHanded(first, log, "a");
  const b = openHanded(second, log, "b");
  first.markAwaitingResults();
  assert.throws(
    () => first.applyResults([{ toolCallId: a.wireId, content: "a" }, { toolCallId: b.wireId, content: "b" }]),
    (error) => error.code === "mixed_tool_rounds",
  );
  assert.equal(first.calls.get(a.wireId).resultHash, null);
});

test("result before transport handoff is refused", () => {
  const round = createTestRound();
  const call = round.openCall({ rawToolCallId: "a", name: "Read", input: { path: "/x" }, callback: callbackLog([], "a") });
  round.markAwaitingResults();
  assert.throws(
    () => round.applyResults([{ toolCallId: call.wireId, content: "too early" }]),
    (error) => error.code === "result_before_handoff",
  );
});

test("terminalization persists then rejects every unresolved callback exactly once", (t) => {
  const dir = withTempDir(t);
  const { journal, codec } = createRoundInfrastructure(dir, { secret: Buffer.alloc(32, 6) });
  const log = [];
  const round = new ToolRound({ sessionId: "sess", journal, codec });
  const a = openHanded(round, log, "a");
  const b = openHanded(round, log, "b");
  let timerClears = 0;
  a.timer = { a: 1 };
  b.timer = { b: 1 };
  round.timers = { clearTimeout() { timerClears++; }, setTimeout };
  assert.equal(round.terminalize(TerminalReason.TRANSPORT_ERROR, "socket closed"), true);
  assert.equal(round.terminalize(TerminalReason.SHUTDOWN, "second terminal"), false);
  assert.equal(timerClears, 2);
  assert.equal(log.filter((entry) => entry[0] === "reject").length, 2);
  const saved = journal.read(round.route);
  assert.equal(saved.state, RoundState.TERMINAL);
  assert.equal(saved.terminal.reason, TerminalReason.TRANSPORT_ERROR);
  assert.ok(saved.calls.every((call) => call.state === CallState.TERMINAL));
});

test("pending watchdog callback terminalizes and clears its timer exactly once", () => {
  const log = [];
  let fire = null;
  let clears = 0;
  const round = createTestRound({
    timers: {
      setTimeout(callback) { fire = callback; return { timer: true }; },
      clearTimeout() { clears++; },
    },
  });
  const call = openHanded(round, log, "watchdog");
  round.markAwaitingResults();
  assert.equal(round.startTimer(call.wireId, 10, () => round.terminalize(TerminalReason.PENDING_TIMEOUT, "watchdog expired")), true);
  fire();
  assert.equal(round.terminal.reason, TerminalReason.PENDING_TIMEOUT);
  assert.equal(log.filter((entry) => entry[0] === "reject").length, 1);
  assert.equal(clears, 0, "the timer has already fired, so terminalization must not clear it a second time");
  assert.equal(round.terminalize(TerminalReason.SHUTDOWN), false);
  assert.equal(log.filter((entry) => entry[0] === "reject").length, 1);
});

test("a committed receipt can be loaded after restart without a fake callback", (t) => {
  const dir = withTempDir(t);
  const { journal, codec } = createRoundInfrastructure(dir, { secret: Buffer.alloc(32, 8) });
  const round = new ToolRound({ sessionId: "sess", agentId: "agent", journal, codec });
  const call = openHanded(round, [], "a");
  round.markAwaitingResults();
  round.commitResults([{ toolCallId: call.wireId, content: "durable" }]);
  const recovered = ToolRound.load(journal, codec, round.route);
  assert.equal(recovered.state, RoundState.APPLYING_RESULTS);
  assert.equal(recovered.callbacks.size, 0);
  assert.equal(recovered.calls.get(call.wireId).receipt.result.content, "durable");
  assert.equal(recovered.calls.get(call.wireId).callbackAppliedAt, null);
});

test("STATE_ROOT probe exercises durable create, fsync, rename, read, and cleanup", (t) => {
  const dir = withTempDir(t);
  assert.deepEqual(probeStateRoot(dir), { ok: true, stateRoot: path.resolve(dir) });
  assert.deepEqual(probeStateRoot(dir), { ok: true, stateRoot: path.resolve(dir) });
});

test("journal retention deletes only terminal receipts and never an open round", (t) => {
  const dir = withTempDir(t);
  let now = 1_000;
  const { journal, codec } = createRoundInfrastructure(dir, { secret: Buffer.alloc(32, 12), now: () => now });
  const oldTerminal = new ToolRound({ sessionId: "old", journal, codec });
  oldTerminal.terminalize(TerminalReason.COMPLETED);
  now = 2_000;
  const recentTerminal = new ToolRound({ sessionId: "recent", journal, codec });
  recentTerminal.terminalize(TerminalReason.COMPLETED);
  const open = new ToolRound({ sessionId: "open", journal, codec });

  const first = journal.cleanupTerminal({ ttlMs: 500, maxTerminal: 10 });
  assert.deepEqual(first, { removed: 1, retained: 1 });
  assert.equal(journal.read(oldTerminal.route), null);
  assert.equal(journal.read(recentTerminal.route).state, RoundState.TERMINAL);
  assert.equal(journal.read(open.route).state, RoundState.COLLECTING);

  const second = journal.cleanupTerminal({ ttlMs: 10_000, maxTerminal: 0 });
  assert.deepEqual(second, { removed: 1, retained: 0 });
  assert.equal(journal.read(recentTerminal.route), null);
  assert.equal(journal.read(open.route).state, RoundState.COLLECTING);
});

test("startup terminalizes orphaned nonterminal rounds so retention can eventually bound them", (t) => {
  const dir = withTempDir(t);
  const { journal, codec } = createRoundInfrastructure(dir, { secret: Buffer.alloc(32, 14) });
  const open = new ToolRound({ sessionId: "orphan", journal, codec });
  const call = open.openCall({ rawToolCallId: "raw", name: "Read", input: { path: "/x" } });
  open.markHanded(call.wireId);
  open.markAwaitingResults();

  assert.equal(terminalizeOrphanedRounds(journal, codec), 1);
  const saved = journal.read(open.route);
  assert.equal(saved.state, RoundState.TERMINAL);
  assert.equal(saved.terminal.reason, TerminalReason.RESTART_LOST);
  assert.equal(terminalizeOrphanedRounds(journal, codec), 0);
});

test("the in-memory receipt is byte-identical to the persisted receipt", (t) => {
  const dir = withTempDir(t);
  let now = 42;
  const { journal, codec } = createRoundInfrastructure(dir, { secret: Buffer.alloc(32, 13), now: () => now++ });
  const round = new ToolRound({ sessionId: "receipt", journal, codec });
  const call = openHanded(round, [], "raw");
  round.markAwaitingResults();
  round.commitResults([{ toolCallId: call.wireId, content: "ok", structuredContent: { exact: true } }]);
  assert.deepEqual(round.calls.get(call.wireId).receipt, journal.read(round.route).calls[0].receipt);
});

test("typed round errors retain status and details", () => {
  const error = new ToolRoundError("x", "problem", 418, { a: 1 });
  assert.equal(error.code, "x");
  assert.equal(error.status, 418);
  assert.deepEqual(error.details, { a: 1 });
});
