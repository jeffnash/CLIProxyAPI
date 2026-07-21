import assert from "node:assert/strict";
import test from "node:test";
import { existsSync, mkdtempSync, rmSync } from "node:fs";
import os from "node:os";
import path from "node:path";

import { AgentLeaseBusyError, DurableAgentLeaseManager } from "./agent-use-lease.mjs";

function manager(t, options = {}) {
  const root = mkdtempSync(path.join(os.tmpdir(), "cct-agent-lease-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  return new DurableAgentLeaseManager(root, {
    heartbeatMs: 60_000,
    lockWaitMs: 20,
    ...options,
  });
}

test("active request use lease fences destructive GC mutation", async (t) => {
  const leases = manager(t);
  const use = await leases.acquireUse("tenant", "agent-a");
  let mutated = false;
  await assert.rejects(
    leases.withMutation({ scope: "tenant", agentId: "agent-a" }, async () => { mutated = true; }),
    (error) => error instanceof AgentLeaseBusyError && error.retryable === true,
  );
  assert.equal(mutated, false);
  await use.release();
  await leases.withMutation({ scope: "tenant", agentId: "agent-a" }, async () => { mutated = true; });
  assert.equal(mutated, true);
});

test("request restore hook and use publication share one mutation lock", async (t) => {
  const leases = manager(t);
  const events = [];
  const use = await leases.acquireUse("tenant", "agent-b", {
    beforeAcquire: async () => { events.push("restore"); },
  });
  events.push("acquired");
  assert.deepEqual(events, ["restore", "acquired"]);
  await use.release();
});

test("alias publication and GC mutation serialize per agent without blocking unrelated agents", async (t) => {
  const leases = manager(t, { lockWaitMs: 1_000 });
  let releasePublication;
  const publicationGate = new Promise((resolve) => { releasePublication = resolve; });
  let publicationStarted;
  const started = new Promise((resolve) => { publicationStarted = resolve; });
  const publication = leases.withLock({ scope: "tenant", agentId: "agent-published" }, async () => {
    publicationStarted();
    await publicationGate;
  });
  await started;

  let targetMutated = false;
  const targetMutation = leases.withMutation({ scope: "tenant", agentId: "agent-published" }, async () => {
    targetMutated = true;
  });
  let otherMutated = false;
  await leases.withMutation({ scope: "tenant", agentId: "agent-unrelated" }, async () => {
    otherMutated = true;
  });
  assert.equal(otherMutated, true, "one agent's publication cannot become a fleet-wide choke point");
  assert.equal(targetMutated, false, "GC cannot cross an in-flight alias publication");

  releasePublication();
  await publication;
  await targetMutation;
  assert.equal(targetMutated, true);
});

test("expired crashed use leases are reclaimed only after the safety grace", async (t) => {
  let now = 1_000;
  const leases = manager(t, { now: () => now, useLeaseMs: 50, useLeaseStaleMs: 0 });
  const use = await leases.acquireUse("tenant", "agent-c");
  now += 100;
  let mutated = false;
  await leases.withMutation({ scope: "tenant", agentId: "agent-c" }, async () => { mutated = true; });
  assert.equal(mutated, true);
  await use.release();
});

test("a missed heartbeat remains fail-closed throughout the stale safety window", async (t) => {
  let now = 1_000;
  const leases = manager(t, { now: () => now, useLeaseMs: 50, useLeaseStaleMs: 1_000 });
  const use = await leases.acquireUse("tenant", "agent-grace");
  now += 100;
  await assert.rejects(
    leases.withMutation({ scope: "tenant", agentId: "agent-grace" }, async () => {}),
    (error) => error instanceof AgentLeaseBusyError,
  );
  now += 1_000;
  let mutated = false;
  await leases.withMutation({ scope: "tenant", agentId: "agent-grace" }, async () => { mutated = true; });
  assert.equal(mutated, true);
  await use.release();
});

test("sustained renewal failure fails closed via onLeaseLost exactly once and release rethrows (P3.2)", async (t) => {
  const leases = manager(t, { heartbeatMs: 10, useLeaseMs: 100 });
  let lostInfo = null;
  let resolveLost;
  const lostPromise = new Promise((resolve) => { resolveLost = resolve; });
  const use = await leases.acquireUse("tenant", "agent-renew-fail", {
    onLeaseLost: async (info) => { lostInfo = info; resolveLost(info); },
  });
  const original = leases.writeUseLease.bind(leases);
  leases.writeUseLease = async () => { throw new Error("disk full"); };
  const info = await Promise.race([
    lostPromise,
    new Promise((_, reject) => setTimeout(() => reject(new Error("onLeaseLost did not fire")), 2000)),
  ]);
  assert.equal(info.agentId, "agent-renew-fail");
  assert.ok(info.consecutiveFailures >= 1, "failure count is reported");
  // Failures continue but the callback must fire exactly once (no abort storm).
  await new Promise((r) => setTimeout(r, 60));
  assert.equal(lostInfo, info, "onLeaseLost fires exactly once");
  leases.writeUseLease = original;
  await assert.rejects(use.release(), /disk full/, "release surfaces the renewal error instead of swallowing it");
});

test("P9: a multi-minute stream stays fenced through healthy renewals; GC proceeds only after lapse + stale grace", async (t) => {
  let now = 1_000_000;
  const leases = manager(t, { now: () => now, useLeaseMs: 10 * 60_000, heartbeatMs: 5, useLeaseStaleMs: 60 * 60_000 });
  const use = await leases.acquireUse("tenant", "long-stream");
  // A 4.5-minute stream with healthy heartbeats: GC must stay fenced the entire time.
  for (let minute = 0.5; minute <= 4.5; minute += 0.5) {
    now += 30_000;
    await new Promise((r) => setTimeout(r, 12)); // several 5ms heartbeat ticks renew at the advanced clock
    await assert.rejects(
      leases.withMutation({ scope: "tenant", agentId: "long-stream" }, async () => {}),
      (error) => error instanceof AgentLeaseBusyError,
      `GC must stay fenced at +${minute} min of an active stream`,
    );
  }
  // Renewals die (disk failure). Even an EXPIRED lease must fence GC through the stale
  // grace — fail closed, never collect a possibly-active agent.
  leases.writeUseLease = async () => { throw new Error("disk gone"); };
  now += 11 * 60_000; // past expiresAt
  await new Promise((r) => setTimeout(r, 12));
  await assert.rejects(
    leases.withMutation({ scope: "tenant", agentId: "long-stream" }, async () => {}),
    (error) => error instanceof AgentLeaseBusyError,
    "expired-but-not-stale lease must still fence GC",
  );
  now += 60 * 60_000 + 60_000; // past the stale safety grace
  await new Promise((r) => setTimeout(r, 12));
  let mutated = false;
  await leases.withMutation({ scope: "tenant", agentId: "long-stream" }, async () => { mutated = true; });
  assert.equal(mutated, true, "after lapse + stale grace GC may proceed");
  await use.release().catch(() => {});
});

test("a stale lock owner's release cannot delete its successor's lock", async (t) => {
  let now = Date.now();
  const leases = manager(t, { now: () => now, lockStaleMs: 50 });
  const lock = leases.paths("tenant", "agent-d").lock;
  const releaseFirst = await leases.acquireLock(lock);
  now += 100;
  const releaseSecond = await leases.acquireLock(lock);
  assert.equal(await releaseFirst(), false);
  assert.equal(existsSync(lock), true, "the successor still owns the canonical lock path");
  assert.equal(await releaseSecond(), true);
  assert.equal(existsSync(lock), false);
});
