import assert from "node:assert/strict";
import { existsSync, mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import test from "node:test";
import { Worker } from "node:worker_threads";
import { collectSdkAgents, localAgentStateDir, sdkAgentGCEnabled } from "./sdk-agent-gc.mjs";

test("SDK agent GC is maintenance-only and requires an explicit opt-in", () => {
  assert.equal(sdkAgentGCEnabled(undefined), false);
  assert.equal(sdkAgentGCEnabled(""), false);
  assert.equal(sdkAgentGCEnabled("0"), false);
  assert.equal(sdkAgentGCEnabled("false"), false);
  assert.equal(sdkAgentGCEnabled("1"), true);
  assert.equal(sdkAgentGCEnabled("TRUE"), true);
  assert.equal(sdkAgentGCEnabled(" on "), true);
});

test("durable-root discovery runs successfully in an isolated worker", async (t) => {
  const root = mkdtempSync(path.join(tmpdir(), "sdk-agent-root-scanner-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const aliases = path.join(root, ".cct-agent-alias");
  mkdirSync(aliases, { recursive: true });
  for (let index = 0; index < 1_000; index++) {
    writeFileSync(path.join(aliases, `${index}.json`), JSON.stringify({ agentId: `agent-${index}` }));
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
  assert.equal(response.roots.length, 1_000);
  assert.equal(Number.isSafeInteger(response.elapsedMs), true);
  assert.ok(heartbeats > 0, "the main event loop remained responsive during the durable scan");
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
  assert.equal(stats.markersCleared, 1);
  assert.equal(stats.deleted, 0);
  assert.equal(platform.values.has("remote-gone"), false);
  assert.equal(existsSync(localDirectory), false);
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
