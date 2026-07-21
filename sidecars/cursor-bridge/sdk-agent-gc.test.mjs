import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { existsSync, mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import test from "node:test";
import { Worker } from "node:worker_threads";
import {
  collectSdkAgents,
  compareSdkAgentsForGC,
  cancelSdkAgentQuarantine,
  gcPressureDecision,
  localAgentStateDir,
  markerPath,
  readMarker,
  writeMarker,
  sdkAgentGCEnabled,
} from "./sdk-agent-gc.mjs";

test("SDK agent GC is maintenance-only and requires an explicit opt-in", () => {
  assert.equal(sdkAgentGCEnabled(undefined), false);
  assert.equal(sdkAgentGCEnabled(""), false);
  assert.equal(sdkAgentGCEnabled("0"), false);
  assert.equal(sdkAgentGCEnabled("false"), false);
  assert.equal(sdkAgentGCEnabled("1"), true);
  assert.equal(sdkAgentGCEnabled("TRUE"), true);
  assert.equal(sdkAgentGCEnabled(" on "), true);
});

// P4: the worker now runs the journal+receipt CENSUS (the audit/rebuild path), not the
// hot-path root discovery. Seed unresolved delivery receipts and assert the census recovers
// their agents off the main event loop.
test("the census worker recovers durable journal/receipt roots without stalling the event loop", async (t) => {
  const root = mkdtempSync(path.join(tmpdir(), "sdk-agent-root-scanner-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const receipts = path.join(root, ".cct-completed-turns");
  mkdirSync(receipts, { recursive: true });
  for (let index = 0; index < 100; index++) {
    const hex = createHash("sha256").update(`receipt-${index}`).digest("hex");
    writeFileSync(path.join(receipts, `${hex}.json`), JSON.stringify({
      version: 1,
      status: "delivering",
      agentId: `receipt-agent-${index}`,
    }));
  }
  const worker = new Worker(new URL("./sdk-agent-root-scanner.mjs", import.meta.url), {
    env: { ...process.env, CURSOR_AGENT_STATE_ROOT: root },
  });
  t.after(() => worker.terminate());
  let heartbeats = 0;
  const heartbeat = setInterval(() => { heartbeats++; }, 1);
  t.after(() => clearInterval(heartbeat));
  const response = await new Promise((resolve, reject) => {
    worker.once("message", resolve);
    worker.once("error", reject);
    worker.postMessage({ id: 1 });
  });
  clearInterval(heartbeat);
  assert.equal(response.id, 1);
  assert.equal(response.roots.length, 100, "every unresolved receipt agent is a census root");
  assert.ok(response.roots.includes("receipt-agent-0"));
  assert.equal(Number.isSafeInteger(response.elapsedMs), true);
  assert.ok(heartbeats > 0, "the main event loop remained responsive during the durable census");
});

function mockPlatform(agents) {
  const values = new Map(agents.map((agent) => [agent.agentId, { ...agent }]));
  const calls = [];
  return {
    calls,
    values,
    async listAgents({ limit, cursor }) {
      const offset = Number(cursor || 0);
      const items = [...values.values()].slice(offset, offset + limit);
      return { items, ...(offset + items.length < values.size ? { nextCursor: String(offset + items.length) } : {}) };
    },
    async getAgent(agentId) {
      const agent = values.get(agentId);
      if (!agent) throw new Error("not found");
      return { ...agent };
    },
    async archiveAgent(agentId) { calls.push(["archive", agentId]); values.get(agentId).archived = true; },
    async unarchiveAgent(agentId) { calls.push(["unarchive", agentId]); values.get(agentId).archived = false; },
    async deleteAgent(agentId) { calls.push(["delete", agentId]); values.delete(agentId); },
  };
}

function options(platform, root, now, protectedAgentIds = new Set()) {
  return {
    platform,
    scope: "tenant-a",
    quarantineRoot: root,
    localStateRoot: path.join(root, "state"),
    protectedAgentIds,
    refreshProtectedAgentIds: () => protectedAgentIds,
    now: () => now.value,
    minIdleMs: 1_000,
    quarantineMs: 5_000,
    maxScan: 100,
    maxMutations: 100,
  };
}

test("an old marker reclaims a local checkpoint after Cursor no longer lists the agent", async (t) => {
  const root = mkdtempSync(path.join(tmpdir(), "sdk-agent-gc-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const now = { value: 20_000 };
  const platform = mockPlatform([{ agentId: "remote-gone", lastModified: 1_000, archived: false }]);
  const opts = options(platform, root, now);
  await collectSdkAgents(opts);

  platform.values.delete("remote-gone");
  const localDirectory = localAgentStateDir(opts.localStateRoot, "remote-gone");
  mkdirSync(localDirectory, { recursive: true });
  writeFileSync(path.join(localDirectory, "store.db"), "checkpoint");
  now.value += 10_000;

  const stats = await collectSdkAgents(opts);
  assert.equal(stats.localDeleted, 1);
  assert.equal(stats.markersCleared, 0);
  assert.equal(stats.deleted, 0);
  assert.equal(platform.values.has("remote-gone"), false);
  assert.equal(existsSync(localDirectory), false);
  assert.equal(readMarker(markerPath(opts.quarantineRoot, opts.scope, "remote-gone")).status, "deleted");
});

test("P6.4: tombstones compact only when the compaction authority proves no handle can name the id", async (t) => {
  const root = mkdtempSync(path.join(tmpdir(), "sdk-agent-gc-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const now = { value: 20_000 };
  const platform = mockPlatform([]);
  const opts = options(platform, root, now);
  mkdirSync(opts.quarantineRoot, { recursive: true });
  const markerFile = markerPath(opts.quarantineRoot, opts.scope, "compacted-agent");
  writeMarker(markerFile, {
    version: 1, scope: opts.scope, agentId: "compacted-agent",
    quarantinedAt: 1_000, status: "deleted", deletedAt: 2_000,
  });

  // Authority denies → the tombstone survives the run (default behavior is never compact).
  await collectSdkAgents({ ...opts, tombstoneCompactable: () => false });
  assert.ok(existsSync(markerFile), "tombstone retained while the compaction authority denies");

  // Authority permits → cleared on the next run.
  const stats = await collectSdkAgents({
    ...opts,
    tombstoneCompactable: (marker) => marker.agentId === "compacted-agent",
  });
  assert.equal(existsSync(markerFile), false, "tombstone compacted once the authority proves safety");
  assert.equal(stats.markersCleared, 1);
});

test("a stale listed agent that vanishes at deletion still reclaims its checkpoint", async (t) => {
  const root = mkdtempSync(path.join(tmpdir(), "sdk-agent-gc-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const now = { value: 20_000 };
  const platform = mockPlatform([{ agentId: "stale-listing", lastModified: 1_000, archived: false }]);
  const opts = options(platform, root, now);
  await collectSdkAgents(opts);
  const localDirectory = localAgentStateDir(opts.localStateRoot, "stale-listing");
  mkdirSync(localDirectory, { recursive: true });
  platform.getAgent = async () => { throw new Error("not found"); };
  now.value += 10_000;

  const stats = await collectSdkAgents(opts);
  assert.equal(stats.localDeleted, 1);
  assert.equal(stats.markersCleared, 0);
  assert.equal(existsSync(localDirectory), false);
  assert.equal(readMarker(markerPath(opts.quarantineRoot, opts.scope, "stale-listing")).status, "deleted");
});

test("a durable root prevents marker-driven local checkpoint deletion", async (t) => {
  const root = mkdtempSync(path.join(tmpdir(), "sdk-agent-gc-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const now = { value: 20_000 };
  const platform = mockPlatform([{ agentId: "still-rooted", lastModified: 1_000, archived: false }]);
  const opts = options(platform, root, now);
  await collectSdkAgents(opts);
  platform.values.delete("still-rooted");
  const localDirectory = localAgentStateDir(opts.localStateRoot, "still-rooted");
  mkdirSync(localDirectory, { recursive: true });
  now.value += 10_000;
  opts.refreshProtectedAgentIds = async () => new Set(["still-rooted"]);

  const stats = await collectSdkAgents(opts);
  assert.equal(stats.localDeleted, 0);
  assert.equal(stats.protected, 1);
  assert.equal(existsSync(localDirectory), true);
});

test("markers from another tenant cannot influence an identical agent id", async (t) => {
  const root = mkdtempSync(path.join(tmpdir(), "sdk-agent-gc-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const now = { value: 20_000 };
  const platform = mockPlatform([{ agentId: "shared-name", lastModified: 1_000, archived: false }]);
  const opts = options(platform, root, now);
  mkdirSync(root, { recursive: true });
  writeMarker(markerPath(root, "tenant-b", "shared-name"), {
    version: 1,
    safetyVersion: 2,
    scope: "tenant-b",
    agentId: "shared-name",
    quarantinedAt: 1_000,
    status: "deleted",
    deletedAt: 2_000,
  });

  const stats = await collectSdkAgents(opts);
  assert.equal(stats.quarantined, 1, "foreign tombstone cannot suppress this tenant's eligible agent");
  assert.deepEqual(platform.calls, [["archive", "shared-name"]]);
  assert.equal(readMarker(markerPath(root, "tenant-a", "shared-name")).status, "quarantined");
  assert.equal(readMarker(markerPath(root, "tenant-b", "shared-name")).status, "deleted");
});

test("an unlisted but remotely active agent cancels marker-driven deletion", async (t) => {
  const root = mkdtempSync(path.join(tmpdir(), "sdk-agent-gc-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const now = { value: 20_000 };
  const platform = mockPlatform([{ agentId: "active-again", lastModified: 1_000, archived: false }]);
  const opts = options(platform, root, now);
  await collectSdkAgents(opts);
  platform.values.get("active-again").archived = false;
  platform.listAgents = async () => ({ items: [] });
  const localDirectory = localAgentStateDir(opts.localStateRoot, "active-again");
  mkdirSync(localDirectory, { recursive: true });
  now.value += 10_000;

  const stats = await collectSdkAgents(opts);
  assert.equal(stats.localDeleted, 0);
  assert.equal(stats.restored, 1);
  assert.equal(existsSync(localDirectory), true);
  assert.equal(platform.values.has("active-again"), true);
});

test("SDK agent GC archives, quarantines, then deletes only after a second TTL", async (t) => {
  const root = mkdtempSync(path.join(tmpdir(), "sdk-agent-gc-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const now = { value: 10_000 };
  const platform = mockPlatform([{ agentId: "old", lastModified: 1_000, archived: false }]);
  let stats = await collectSdkAgents(options(platform, root, now));
  assert.equal(stats.quarantined, 1);
  assert.deepEqual(platform.calls, [["archive", "old"]]);

  now.value += 4_999;
  stats = await collectSdkAgents(options(platform, root, now));
  assert.equal(stats.deleted, 0);

  now.value += 1;
  stats = await collectSdkAgents(options(platform, root, now));
  assert.equal(stats.deleted, 1);
  assert.deepEqual(platform.calls, [["archive", "old"], ["delete", "old"]]);
  assert.equal(readMarker(markerPath(root, "tenant-a", "old")).status, "deleted",
    "a successful remote delete must retain a continuity tombstone");
  const resume = await cancelSdkAgentQuarantine({
    platform, scope: "tenant-a", quarantineRoot: root, agentId: "old",
  });
  assert.deepEqual(resume, { restored: false, missing: true },
    "a stale alias must observe collection and choose bounded reseed or 410, never blank create");
  assert.equal(readMarker(markerPath(root, "tenant-a", "old")).status, "deleted");
});

test("legacy quarantine markers receive a fresh safe TTL before deletion is eligible", async (t) => {
  const root = mkdtempSync(path.join(tmpdir(), "sdk-agent-gc-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const now = { value: 20_000 };
  const platform = mockPlatform([{ agentId: "legacy", lastModified: 1_000, archived: true }]);
  const opts = options(platform, root, now);
  mkdirSync(root, { recursive: true });
  const file = markerPath(root, opts.scope, "legacy");
  writeMarker(file, {
    version: 1,
    scope: opts.scope,
    agentId: "legacy",
    quarantinedAt: 1_000,
    status: "quarantined",
  });

  let stats = await collectSdkAgents(opts);
  assert.equal(stats.requarantined, 1);
  assert.equal(stats.deleted, 0, "an inherited marker cannot delete on its first safe-GC pass");
  assert.deepEqual(platform.calls, []);
  assert.equal(readMarker(file).quarantinedAt, 20_000);
  assert.equal(readMarker(file).safetyVersion, 2);

  now.value += 4_999;
  stats = await collectSdkAgents(opts);
  assert.equal(stats.deleted, 0);
  now.value += 1;
  stats = await collectSdkAgents(opts);
  assert.equal(stats.deleted, 1);
  assert.deepEqual(platform.calls, [["delete", "legacy"]]);
});

test("final lifecycle mutation remains inside the per-agent mutation fence", async (t) => {
  const root = mkdtempSync(path.join(tmpdir(), "sdk-agent-gc-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const now = { value: 20_000 };
  const platform = mockPlatform([{ agentId: "publication-fenced", lastModified: 1_000, archived: false }]);
  let mutationFenceHeld = false;
  const originalArchive = platform.archiveAgent;
  platform.archiveAgent = async (agentId) => {
    assert.equal(mutationFenceHeld, true, "archive must not escape the per-agent mutation fence");
    return originalArchive(agentId);
  };

  const stats = await collectSdkAgents({
    ...options(platform, root, now),
    withAgentMutationLease: async (_context, mutate) => {
      assert.equal(mutationFenceHeld, false);
      mutationFenceHeld = true;
      try { return await mutate(); }
      finally { mutationFenceHeld = false; }
    },
  });
  assert.equal(stats.quarantined, 1);
  assert.equal(mutationFenceHeld, false);
});

test("a request that restores after GC census but before its mutation lock cancels deletion", async (t) => {
  const root = mkdtempSync(path.join(tmpdir(), "sdk-agent-gc-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const now = { value: 20_000 };
  const platform = mockPlatform([{ agentId: "lease-race", lastModified: 1_000, archived: false }]);
  const opts = options(platform, root, now);
  await collectSdkAgents(opts);
  now.value += 10_000;
  const file = markerPath(root, "tenant-a", "lease-race");
  opts.withAgentMutationLease = async (context, mutate) => {
    if (context.operation === "delete") {
      platform.values.get("lease-race").archived = false;
      rmSync(file, { force: true });
    }
    return mutate();
  };

  const stats = await collectSdkAgents(opts);
  assert.equal(stats.deleted, 0);
  assert.equal(platform.values.has("lease-race"), true);
  assert.deepEqual(platform.calls, [["archive", "lease-race"]]);
});

test("protected agents are never archived and quarantined agents are restored when referenced", async (t) => {
  const root = mkdtempSync(path.join(tmpdir(), "sdk-agent-gc-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const now = { value: 20_000 };
  const platform = mockPlatform([
    { agentId: "always-protected", lastModified: 1_000, archived: false },
    { agentId: "becomes-protected", lastModified: 1_000, archived: false },
  ]);
  const roots = new Set(["always-protected"]);
  await collectSdkAgents(options(platform, root, now, roots));
  assert.deepEqual(platform.calls, [["archive", "becomes-protected"]]);

  roots.add("becomes-protected");
  now.value += 10_000;
  const stats = await collectSdkAgents(options(platform, root, now, roots));
  assert.equal(stats.restored, 1);
  assert.deepEqual(platform.calls, [["archive", "becomes-protected"], ["unarchive", "becomes-protected"]]);
});

test("a root appearing during the pre-mutation recheck fences archival", async (t) => {
  const root = mkdtempSync(path.join(tmpdir(), "sdk-agent-gc-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const now = { value: 20_000 };
  const platform = mockPlatform([{ agentId: "racing", lastModified: 1_000, archived: false }]);
  let checks = 0;
  const opts = options(platform, root, now);
  opts.refreshProtectedAgentIds = () => (++checks >= 1 ? new Set(["racing"]) : new Set());
  const stats = await collectSdkAgents(opts);
  assert.equal(stats.quarantined, 0);
  assert.deepEqual(platform.calls, []);
});

test("an asynchronous durable-root refresh fences archival", async (t) => {
  const root = mkdtempSync(path.join(tmpdir(), "sdk-agent-gc-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const now = { value: 20_000 };
  const platform = mockPlatform([{ agentId: "async-root", lastModified: 1_000, archived: false }]);
  const opts = options(platform, root, now);
  opts.refreshProtectedAgentIds = async () => new Set(["async-root"]);
  const stats = await collectSdkAgents(opts);
  assert.equal(stats.quarantined, 0);
  assert.deepEqual(platform.calls, []);
});

test("an externally unarchived agent cancels quarantine instead of being deleted", async (t) => {
  const root = mkdtempSync(path.join(tmpdir(), "sdk-agent-gc-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const now = { value: 20_000 };
  const platform = mockPlatform([{ agentId: "reused", lastModified: 1_000, archived: false }]);
  await collectSdkAgents(options(platform, root, now));
  platform.values.get("reused").archived = false;
  now.value += 10_000;
  const stats = await collectSdkAgents(options(platform, root, now));
  assert.equal(stats.deleted, 0);
  assert.equal(stats.restored, 1);
  assert.deepEqual(platform.calls, [["archive", "reused"]]);
});

test("an agent unarchived after listing is rechecked and never deleted", async (t) => {
  const root = mkdtempSync(path.join(tmpdir(), "sdk-agent-gc-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const now = { value: 20_000 };
  const platform = mockPlatform([{ agentId: "racing", lastModified: 1_000, archived: false }]);
  await collectSdkAgents(options(platform, root, now));
  now.value += 10_000;
  const getAgent = platform.getAgent;
  platform.getAgent = async (agentId) => {
    platform.values.get(agentId).archived = false;
    return getAgent(agentId);
  };
  const stats = await collectSdkAgents(options(platform, root, now));
  assert.equal(stats.deleted, 0);
  assert.equal(stats.restored, 1);
  assert.deepEqual(platform.calls, [["archive", "racing"]]);
});

test("a durable root appearing during deletion preflight fences deletion", async (t) => {
  const root = mkdtempSync(path.join(tmpdir(), "sdk-agent-gc-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const now = { value: 20_000 };
  const platform = mockPlatform([{ agentId: "rooted-late", lastModified: 1_000, archived: false }]);
  await collectSdkAgents(options(platform, root, now));
  now.value += 10_000;
  let checks = 0;
  const opts = options(platform, root, now);
  opts.refreshProtectedAgentIds = () => (++checks >= 2 ? new Set(["rooted-late"]) : new Set());
  const stats = await collectSdkAgents(opts);
  assert.equal(stats.deleted, 0);
  assert.equal(stats.protected, 1);
  assert.deepEqual(platform.calls, [["archive", "rooted-late"]]);
});

test("an incomplete initial root snapshot aborts before listing or mutation", async (t) => {
  const root = mkdtempSync(path.join(tmpdir(), "sdk-agent-gc-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const platform = mockPlatform([{ agentId: "must-survive", lastModified: 1, status: "ERROR" }]);
  platform.listAgents = async () => { throw new Error("must not list after incomplete roots"); };
  const opts = options(platform, root, { value: 20_000 });
  opts.protectedAgentIds = { complete: false, roots: [] };

  const stats = await collectSdkAgents(opts);
  assert.equal(stats.aborted, true);
  assert.equal(stats.incompleteRootSnapshots, 1);
  assert.deepEqual(platform.calls, []);
});

test("an incomplete final root snapshot fails closed before archival", async (t) => {
  const root = mkdtempSync(path.join(tmpdir(), "sdk-agent-gc-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const platform = mockPlatform([{ agentId: "uncertain", lastModified: 1, status: "IDLE" }]);
  const opts = options(platform, root, { value: 20_000 });
  let scans = 0;
  opts.protectedAgentIds = { complete: true, roots: [], generation: 1 };
  opts.refreshProtectedAgentIds = () => (++scans === 1
    ? { complete: true, roots: [], generation: 2 }
    : { complete: false, roots: [], generation: 3 });

  const stats = await collectSdkAgents(opts);
  assert.equal(stats.aborted, true);
  assert.equal(stats.incompleteRootSnapshots, 1);
  assert.deepEqual(platform.calls, []);
});

test("a root acquired at the final-check hook fences the mutation", async (t) => {
  const root = mkdtempSync(path.join(tmpdir(), "sdk-agent-gc-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const platform = mockPlatform([{ agentId: "claimed-during-fence", lastModified: 1, status: "IDLE" }]);
  const roots = new Set();
  const opts = options(platform, root, { value: 20_000 });
  opts.refreshProtectedAgentIds = () => ({ complete: true, roots, generation: roots.size });
  opts.beforeMutation = async ({ agentId, operation }) => {
    assert.equal(operation, "archive");
    roots.add(agentId);
  };

  const stats = await collectSdkAgents(opts);
  assert.equal(stats.protected, 1);
  assert.deepEqual(platform.calls, []);
});

test("a shared generation validator fences mutation after the final root scan", async (t) => {
  const root = mkdtempSync(path.join(tmpdir(), "sdk-agent-gc-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const platform = mockPlatform([{ agentId: "generation-race", lastModified: 1, status: "IDLE" }]);
  const opts = options(platform, root, { value: 20_000 });
  opts.refreshProtectedAgentIds = () => ({ complete: true, roots: [], generation: 7 });
  opts.validateMutationGeneration = async ({ generation }) => generation !== 7;

  const stats = await collectSdkAgents(opts);
  assert.equal(stats.fenced, 1);
  assert.deepEqual(platform.calls, []);
});

test("GC ordering is deterministic, status-aware, and oldest-first within status", async (t) => {
  const agents = [
    { agentId: "running-old", lastModified: 1, status: "RUNNING" },
    { agentId: "idle-new", lastModified: 5, status: "IDLE" },
    { agentId: "error-new", lastModified: 9, status: "ERROR" },
    { agentId: "idle-old", lastModified: 2, status: "IDLE" },
  ];
  assert.deepEqual([...agents].sort(compareSdkAgentsForGC).map((agent) => agent.agentId),
    ["error-new", "idle-old", "idle-new", "running-old"]);

  const root = mkdtempSync(path.join(tmpdir(), "sdk-agent-gc-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const platform = mockPlatform(agents);
  const opts = options(platform, root, { value: 20_000 });
  opts.maxMutations = 2;
  await collectSdkAgents(opts);
  assert.deepEqual(platform.calls, [["archive", "error-new"], ["archive", "idle-old"]]);
});

test("bounded scans expose a continuation cursor so successive runs are fair", async (t) => {
  const root = mkdtempSync(path.join(tmpdir(), "sdk-agent-gc-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const agents = Array.from({ length: 5 }, (_, index) => ({
    agentId: `agent-${index}`, lastModified: 19_999, status: "IDLE",
  }));
  const platform = mockPlatform(agents);
  const now = { value: 20_000 };
  const first = options(platform, root, now);
  first.maxScan = 2;
  first.minIdleMs = 10_000;
  const firstStats = await collectSdkAgents(first);
  assert.equal(firstStats.scanned, 2);
  assert.equal(firstStats.nextScanCursor, "2");

  const second = { ...first, scanCursor: firstStats.nextScanCursor };
  const secondStats = await collectSdkAgents(second);
  assert.equal(secondStats.scanned, 2);
  assert.equal(secondStats.nextScanCursor, "4");
});

test("pressure hysteresis stops below low water and starts at high water", async (t) => {
  assert.equal(gcPressureDecision({ usedBytes: 90, highWaterBytes: 80, lowWaterBytes: 60 }).active, true);
  assert.equal(gcPressureDecision({ usedBytes: 70, highWaterBytes: 80, lowWaterBytes: 60 }).active, false);
  assert.equal(gcPressureDecision({ usedBytes: 70, highWaterBytes: 80, lowWaterBytes: 60, active: true }).active, true);
  assert.equal(gcPressureDecision({ usedBytes: 60, highWaterBytes: 80, lowWaterBytes: 60, active: true }).active, false);

  const root = mkdtempSync(path.join(tmpdir(), "sdk-agent-gc-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const platform = mockPlatform([{ agentId: "pressure-candidate", lastModified: 1, status: "ERROR" }]);
  const opts = options(platform, root, { value: 20_000 });
  opts.pressure = { usedBytes: 70, highWaterBytes: 80, lowWaterBytes: 60 };
  const inactive = await collectSdkAgents(opts);
  assert.equal(inactive.pressureActive, false);
  assert.deepEqual(platform.calls, []);

  opts.pressure = { ...opts.pressure, usedBytes: 90 };
  opts.maxMutations = 1;
  const active = await collectSdkAgents(opts);
  assert.equal(active.pressureActive, true);
  assert.deepEqual(platform.calls, [["archive", "pressure-candidate"]]);
});

test("a corrupt quarantine marker fails before any agent mutation", async (t) => {
  const root = mkdtempSync(path.join(tmpdir(), "sdk-agent-gc-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  mkdirSync(root, { recursive: true });
  writeFileSync(path.join(root, `${"a".repeat(64)}.json`), "{broken");
  const platform = mockPlatform([{ agentId: "must-not-mutate", lastModified: 1, status: "ERROR" }]);
  await assert.rejects(collectSdkAgents(options(platform, root, { value: 20_000 })), /JSON|Unexpected/);
  assert.deepEqual(platform.calls, []);
});

test("request-side quarantine cancellation restores under the shared agent lease", async (t) => {
  const root = mkdtempSync(path.join(tmpdir(), "sdk-agent-gc-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const now = { value: 20_000 };
  const platform = mockPlatform([{ agentId: "returning", lastModified: 1, status: "IDLE" }]);
  await collectSdkAgents(options(platform, root, now));
  assert.deepEqual(platform.calls, [["archive", "returning"]]);

  const leaseOperations = [];
  const result = await cancelSdkAgentQuarantine({
    platform,
    scope: "tenant-a",
    quarantineRoot: root,
    agentId: "returning",
    withAgentMutationLease: async (context, restore) => {
      leaseOperations.push(context);
      return restore();
    },
  });
  assert.deepEqual(result, { restored: true, missing: false });
  assert.deepEqual(leaseOperations, [{ scope: "tenant-a", agentId: "returning", operation: "cancel-quarantine" }]);
  assert.deepEqual(platform.calls, [["archive", "returning"], ["unarchive", "returning"]]);

  now.value += 10_000;
  const after = await collectSdkAgents(options(platform, root, now, new Set(["returning"])));
  assert.equal(after.restored, 0, "the request path removed the quarantine marker after restoration");
});

test("a missing quarantined agent preserves its marker for reseed-or-410 handling", async (t) => {
  const root = mkdtempSync(path.join(tmpdir(), "sdk-agent-gc-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const now = { value: 20_000 };
  const platform = mockPlatform([{ agentId: "missing-return", lastModified: 1, status: "IDLE" }]);
  await collectSdkAgents(options(platform, root, now));
  platform.values.delete("missing-return");

  const result = await cancelSdkAgentQuarantine({
    platform, scope: "tenant-a", quarantineRoot: root, agentId: "missing-return",
  });
  assert.deepEqual(result, { restored: false, missing: true });

  now.value += 10_000;
  const after = await collectSdkAgents(options(platform, root, now, new Set(["missing-return"])));
  assert.equal(after.protected, 1, "the preserved marker remains visible to GC and can be fenced by a hard root");
});
