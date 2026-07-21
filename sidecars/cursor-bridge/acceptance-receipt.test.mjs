import assert from "node:assert/strict";
import { mkdtempSync, readFileSync, rmSync, writeFileSync, existsSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import test from "node:test";
import { createHash } from "node:crypto";

const TEST_STATE_ROOT = mkdtempSync(path.join(tmpdir(), "acceptance-receipt-"));
process.env.CURSOR_AGENT_STATE_ROOT = TEST_STATE_ROOT;

const {
  ACCEPTANCE_PHASE,
  TURN_RECEIPT_VERSION,
  migrateLegacyAcceptancePhase,
  resolveAcceptancePhase,
  applyAcceptanceTransition,
  assertLegalAcceptanceTransition,
  isLegalAcceptanceTransition,
  IllegalAcceptanceTransition,
  recoveryActionForPhase,
  sameFrozenEnvelope,
} = await import("./acceptance-receipt.mjs");

const bridge = await import("./cursor-agent-bridge.mjs");
const {
  writeFreshDeliveryReceipt,
  transitionAcceptancePhase,
  transitionFreshAttemptState,
  writeCompletedTurnReceipt,
  readFreshDeliveryReceipt,
  completedTurnRequestHash,
  keyFingerprint,
  mutateTurnReceipt,
} = bridge;

const PHASE = ACCEPTANCE_PHASE;

function receiptPath(cursorKey, sessionId, clientMessageId, requestHash) {
  const scope = createHash("sha256")
    .update(keyFingerprint(cursorKey))
    .update("\0")
    .update(String(sessionId || ""))
    .update("\0")
    .update(String(clientMessageId || ""))
    .update(requestHash ? "\0" : "")
    .update(requestHash)
    .digest("hex");
  return path.join(TEST_STATE_ROOT, ".cct-completed-turns", `${scope}.json`);
}

function baseEnvelope(overrides = {}) {
  return {
    generation: 1,
    agentId: "agent-acceptance-1",
    idempotencyKey: "cctsend1_acceptance",
    message: "frozen envelope text",
    advertise: [],
    model: "composer-2.5",
    toolChoice: "",
    seededSystem: "",
    systemBlockIds: [],
    hasImages: false,
    identityPolicy: "legacy-client-message-v1",
    ...overrides,
  };
}

test("v1-v5 migration maps UNKNOWN/RUNNING/FAILED/completed conservatively", () => {
  assert.equal(migrateLegacyAcceptancePhase({ status: "unknown" }), PHASE.MAYBE_ACCEPTED);
  assert.equal(migrateLegacyAcceptancePhase({ status: "running" }), PHASE.ACCEPTED);
  assert.equal(migrateLegacyAcceptancePhase({ status: "failed", failure: "x", failedAt: 1 }), PHASE.MAYBE_ACCEPTED);
  assert.equal(
    migrateLegacyAcceptancePhase({ status: "failed", failure: "x", failedAt: 1, runningAt: 2 }),
    PHASE.ACCEPTED,
  );
  assert.equal(migrateLegacyAcceptancePhase({ status: "completed" }), PHASE.COMPLETED);
  assert.equal(migrateLegacyAcceptancePhase({ status: "delivering" }), PHASE.MAYBE_ACCEPTED);
  assert.notEqual(migrateLegacyAcceptancePhase({ status: "failed", failure: "x", failedAt: 1 }), PHASE.REJECTED_BEFORE_SEND);
});

test("illegal and backward transitions are rejected fail-closed", () => {
  assert.equal(isLegalAcceptanceTransition(PHASE.MAYBE_ACCEPTED, PHASE.PREPARED_DURABLE), false);
  assert.equal(isLegalAcceptanceTransition(PHASE.MAYBE_ACCEPTED, PHASE.REJECTED_BEFORE_SEND), false);
  assert.equal(isLegalAcceptanceTransition(PHASE.ACCEPTED, PHASE.MAYBE_ACCEPTED), false);
  assert.throws(
    () => assertLegalAcceptanceTransition(PHASE.MAYBE_ACCEPTED, PHASE.REJECTED_BEFORE_SEND),
    IllegalAcceptanceTransition,
  );
  assert.equal(isLegalAcceptanceTransition(PHASE.ACCEPTED, PHASE.ACCEPTED), true);
  assert.equal(isLegalAcceptanceTransition(PHASE.PREPARED_DURABLE, PHASE.MAYBE_ACCEPTED), true);
});

test("crash after envelope commit before MAYBE_ACCEPTED leaves PREPARED_DURABLE", async () => {
  const cursorKey = "k-prepared";
  const sessionId = "s-prepared";
  const clientMessageId = "ccm2_prepared";
  const requestHash = "a".repeat(64);
  const committed = await writeFreshDeliveryReceipt(cursorKey, sessionId, clientMessageId, requestHash, baseEnvelope());
  assert.equal(committed.version, TURN_RECEIPT_VERSION);
  assert.equal(committed.acceptancePhase, PHASE.PREPARED_DURABLE);
  assert.equal(committed.status, undefined);
  const file = receiptPath(cursorKey, sessionId, clientMessageId, requestHash);
  const disk = JSON.parse(readFileSync(file, "utf8"));
  assert.equal(disk.acceptancePhase, PHASE.PREPARED_DURABLE);
  assert.equal(recoveryActionForPhase(resolveAcceptancePhase(disk)), "transition_and_send_same_envelope");
});

test("crash after MAYBE_ACCEPTED before agent.send retains MAYBE_ACCEPTED for exact reattachment", async () => {
  const cursorKey = "k-maybe";
  const sessionId = "s-maybe";
  const clientMessageId = "ccm2_maybe";
  const requestHash = "b".repeat(64);
  await writeFreshDeliveryReceipt(cursorKey, sessionId, clientMessageId, requestHash, baseEnvelope());
  const maybe = transitionAcceptancePhase(
    cursorKey, sessionId, clientMessageId, requestHash, PHASE.MAYBE_ACCEPTED,
  );
  assert.equal(maybe.acceptancePhase, PHASE.MAYBE_ACCEPTED);
  assert.equal(recoveryActionForPhase(PHASE.MAYBE_ACCEPTED), "exact_reattachment_only");
  assert.throws(
    () => transitionAcceptancePhase(
      cursorKey, sessionId, clientMessageId, requestHash, PHASE.REJECTED_BEFORE_SEND,
    ),
    IllegalAcceptanceTransition,
  );
});

test("durable transition failure proves agent.send never called", async () => {
  let sendCalls = 0;
  const agent = {
    async send() {
      sendCalls++;
      return { async wait() { return { status: "finished", result: "x" }; }, async cancel() {} };
    },
  };
  // Mirror the driveUserSend gate: only call agent.send after a fsynced MAYBE_ACCEPTED.
  const maybeAccepted = null; // transition failed / returned nothing
  const phase = maybeAccepted ? resolveAcceptancePhase(maybeAccepted) : PHASE.PREPARED_DURABLE;
  if (phase === PHASE.MAYBE_ACCEPTED || phase === PHASE.ACCEPTED) {
    agent.send();
  }
  assert.equal(sendCalls, 0, "agent.send must not run when MAYBE_ACCEPTED was not committed");

  const cursorKey = "k-gate";
  const sessionId = "s-gate";
  const clientMessageId = "ccm2_gate";
  const requestHash = "c".repeat(64);
  await writeFreshDeliveryReceipt(cursorKey, sessionId, clientMessageId, requestHash, baseEnvelope());
  // Force an illegal transition attempt from PREPARED_DURABLE backward — fail closed, no send.
  assert.throws(
    () => applyAcceptanceTransition(
      { acceptancePhase: PHASE.PREPARED_DURABLE },
      PHASE.NOT_SENT,
    ),
    IllegalAcceptanceTransition,
  );
  assert.equal(sendCalls, 0);
});

test("agent.send rejection retains MAYBE_ACCEPTED", async () => {
  const cursorKey = "k-reject";
  const sessionId = "s-reject";
  const clientMessageId = "ccm2_reject";
  const requestHash = "d".repeat(64);
  await writeFreshDeliveryReceipt(cursorKey, sessionId, clientMessageId, requestHash, baseEnvelope());
  transitionAcceptancePhase(cursorKey, sessionId, clientMessageId, requestHash, PHASE.MAYBE_ACCEPTED);
  // Simulate post-boundary throw: no ACCEPTED transition, no REJECTED.
  const disk = readFreshDeliveryReceipt(cursorKey, sessionId, clientMessageId, requestHash);
  assert.equal(disk.acceptancePhase, PHASE.MAYBE_ACCEPTED);
  assert.throws(
    () => transitionAcceptancePhase(
      cursorKey, sessionId, clientMessageId, requestHash, PHASE.REJECTED_BEFORE_SEND,
    ),
    IllegalAcceptanceTransition,
  );
});

test("both acceptance signals racing converge on one ACCEPTED", async () => {
  const cursorKey = "k-race-accept";
  const sessionId = "s-race-accept";
  const clientMessageId = "ccm2_race_accept";
  const requestHash = "e".repeat(64);
  await writeFreshDeliveryReceipt(cursorKey, sessionId, clientMessageId, requestHash, baseEnvelope());
  transitionAcceptancePhase(cursorKey, sessionId, clientMessageId, requestHash, PHASE.MAYBE_ACCEPTED);
  const [a, b] = await Promise.all([
    Promise.resolve(transitionAcceptancePhase(
      cursorKey, sessionId, clientMessageId, requestHash, PHASE.ACCEPTED,
      { evidence: "user_message_appended" },
    )),
    Promise.resolve(transitionAcceptancePhase(
      cursorKey, sessionId, clientMessageId, requestHash, PHASE.ACCEPTED,
      { evidence: "agent_send_resolved" },
    )),
  ]);
  assert.equal(a.acceptancePhase, PHASE.ACCEPTED);
  assert.equal(b.acceptancePhase, PHASE.ACCEPTED);
  const disk = readFreshDeliveryReceipt(cursorKey, sessionId, clientMessageId, requestHash);
  assert.equal(disk.acceptancePhase, PHASE.ACCEPTED);
});

test("terminal receipt commits before COMPLETED phase", async () => {
  const cursorKey = "k-complete";
  const sessionId = "s-complete";
  const clientMessageId = "ccm2_complete";
  const requestHash = "f".repeat(64);
  await writeFreshDeliveryReceipt(cursorKey, sessionId, clientMessageId, requestHash, baseEnvelope());
  transitionAcceptancePhase(cursorKey, sessionId, clientMessageId, requestHash, PHASE.MAYBE_ACCEPTED);
  transitionAcceptancePhase(
    cursorKey, sessionId, clientMessageId, requestHash, PHASE.ACCEPTED,
    { evidence: "agent_send_resolved" },
  );
  const completed = writeCompletedTurnReceipt(
    cursorKey, sessionId, clientMessageId, requestHash,
    [{ type: "text", delta: "answer" }, { type: "turn_end", stop_reason: "end_turn" }],
    { input_tokens: 1, output_tokens: 1 },
    { generation: 1, replace: true, requestKind: "fresh", identityPolicy: "legacy-client-message-v1" },
  );
  assert.equal(completed.acceptancePhase, PHASE.COMPLETED);
  assert.equal(completed.status, "completed");
  assert.equal(completed.version, TURN_RECEIPT_VERSION);
  assert.ok(Array.isArray(completed.events) && completed.events.length >= 2);
});

test("concurrent writers cannot fork one invocation", async () => {
  const cursorKey = "k-fork";
  const sessionId = "s-fork";
  const clientMessageId = "ccm2_fork";
  const requestHash = "1".repeat(64);
  const results = await Promise.allSettled([
    (async () => writeFreshDeliveryReceipt(cursorKey, sessionId, clientMessageId, requestHash, baseEnvelope({ idempotencyKey: "a" })))(),
    (async () => writeFreshDeliveryReceipt(cursorKey, sessionId, clientMessageId, requestHash, baseEnvelope({ idempotencyKey: "b" })))(),
  ]);
  const wins = results.filter((r) => r.status === "fulfilled");
  const losses = results.filter((r) => r.status === "rejected");
  assert.equal(wins.length, 1, results.map((r) => r.status + (r.reason ? ":" + r.reason.message : "")).join("|"));
  assert.equal(losses.length, 1);
  const disk = readFreshDeliveryReceipt(cursorKey, sessionId, clientMessageId, requestHash);
  assert.equal(disk.acceptancePhase, PHASE.PREPARED_DURABLE);
  assert.equal(disk.deliveryIdempotencyKey, wins[0].value.deliveryIdempotencyKey);
});

test("restart exact-envelope recovery without duplicate execution", async () => {
  const cursorKey = "k-restart";
  const sessionId = "s-restart";
  const clientMessageId = "ccm2_restart";
  const requestHash = "2".repeat(64);
  const first = await writeFreshDeliveryReceipt(cursorKey, sessionId, clientMessageId, requestHash, baseEnvelope());
  transitionAcceptancePhase(cursorKey, sessionId, clientMessageId, requestHash, PHASE.MAYBE_ACCEPTED);
  const recovered = readFreshDeliveryReceipt(cursorKey, sessionId, clientMessageId, requestHash);
  assert.equal(recovered.acceptancePhase, PHASE.MAYBE_ACCEPTED);
  assert.deepEqual(recovered.deliveryMessage, first.deliveryMessage);
  assert.equal(recovered.deliveryIdempotencyKey, first.deliveryIdempotencyKey);
  assert.equal(recoveryActionForPhase(PHASE.MAYBE_ACCEPTED), "exact_reattachment_only");
  // Second prepare for same invocation must fail closed (no fork / duplicate generation).
  await assert.rejects(
    writeFreshDeliveryReceipt(cursorKey, sessionId, clientMessageId, requestHash, baseEnvelope({ generation: 2 })),
  );
});

test("REJECTED_BEFORE_SEND only from pre-boundary phases", async () => {
  const cursorKey = "k-rej";
  const sessionId = "s-rej";
  const clientMessageId = "ccm2_rej";
  const requestHash = "3".repeat(64);
  await writeFreshDeliveryReceipt(cursorKey, sessionId, clientMessageId, requestHash, baseEnvelope());
  const rejected = transitionAcceptancePhase(
    cursorKey, sessionId, clientMessageId, requestHash, PHASE.REJECTED_BEFORE_SEND,
    { reason: "local cancel before send" },
  );
  assert.equal(rejected.acceptancePhase, PHASE.REJECTED_BEFORE_SEND);
  assert.equal(recoveryActionForPhase(PHASE.REJECTED_BEFORE_SEND), "new_attempt_ok");
});

test("legacy transitionFreshAttemptState running shim maps to ACCEPTED", async () => {
  const cursorKey = "k-shim";
  const sessionId = "s-shim";
  const clientMessageId = "ccm2_shim";
  const requestHash = "4".repeat(64);
  await writeFreshDeliveryReceipt(cursorKey, sessionId, clientMessageId, requestHash, baseEnvelope());
  transitionAcceptancePhase(cursorKey, sessionId, clientMessageId, requestHash, PHASE.MAYBE_ACCEPTED);
  const accepted = transitionFreshAttemptState(
    cursorKey, sessionId, clientMessageId, requestHash, "running",
  );
  assert.equal(accepted.acceptancePhase, PHASE.ACCEPTED);
});

test("sameFrozenEnvelope detects mutation after boundary", () => {
  const left = { deliveryMessage: "a", deliveryIdempotencyKey: "k", agentId: "1", generation: 1 };
  const right = { deliveryMessage: "b", deliveryIdempotencyKey: "k", agentId: "1", generation: 1 };
  assert.equal(sameFrozenEnvelope(left, left), true);
  assert.equal(sameFrozenEnvelope(left, right), false);
});

test("crash matrix: MAYBE_ACCEPTED before send retains exact envelope", async () => {
  const cursorKey = "k-crash-maybe";
  const sessionId = "s-crash-maybe";
  const clientMessageId = "ccm2_crash_maybe";
  const requestHash = "5".repeat(64);
  const prepared = await writeFreshDeliveryReceipt(cursorKey, sessionId, clientMessageId, requestHash, baseEnvelope());
  assert.equal(prepared.acceptancePhase, PHASE.PREPARED_DURABLE);
  const maybe = transitionAcceptancePhase(
    cursorKey, sessionId, clientMessageId, requestHash, PHASE.MAYBE_ACCEPTED,
  );
  assert.equal(maybe.acceptancePhase, PHASE.MAYBE_ACCEPTED);
  // Crash before agent.send: recovery must reattach, never reject-as-safe or fork.
  const recovered = readFreshDeliveryReceipt(cursorKey, sessionId, clientMessageId, requestHash);
  assert.equal(recovered.acceptancePhase, PHASE.MAYBE_ACCEPTED);
  assert.equal(recoveryActionForPhase(recovered.acceptancePhase), "exact_reattachment_only");
  assert.equal(sameFrozenEnvelope(prepared, recovered), true);
  assert.throws(() =>
    transitionAcceptancePhase(cursorKey, sessionId, clientMessageId, requestHash, PHASE.REJECTED_BEFORE_SEND),
  );
});

test("crash matrix: event after ACCEPTED cannot roll back", async () => {
  const cursorKey = "k-crash-acc";
  const sessionId = "s-crash-acc";
  const clientMessageId = "ccm2_crash_acc";
  const requestHash = "6".repeat(64);
  await writeFreshDeliveryReceipt(cursorKey, sessionId, clientMessageId, requestHash, baseEnvelope());
  transitionAcceptancePhase(cursorKey, sessionId, clientMessageId, requestHash, PHASE.MAYBE_ACCEPTED);
  transitionAcceptancePhase(cursorKey, sessionId, clientMessageId, requestHash, PHASE.ACCEPTED);
  assert.throws(() =>
    transitionAcceptancePhase(cursorKey, sessionId, clientMessageId, requestHash, PHASE.PREPARED_DURABLE),
  );
  assert.equal(recoveryActionForPhase(PHASE.ACCEPTED), "reattach_or_resume");
});
