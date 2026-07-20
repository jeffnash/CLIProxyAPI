import { createHash, randomUUID } from "node:crypto";
import {
  closeSync,
  fsyncSync,
  mkdirSync,
  openSync,
  readFileSync,
  renameSync,
  rmSync,
  writeSync,
} from "node:fs";
import { lstat, readFile, readdir, rm } from "node:fs/promises";
import path from "node:path";

const RECORD_VERSION = 1;

export function sdkAgentGCEnabled(raw) {
  return ["1", "true", "on"].includes(String(raw || "").trim().toLowerCase());
}

function markerPath(root, scope, agentId) {
  const key = createHash("sha256").update(String(scope)).update("\0").update(String(agentId)).digest("hex");
  return path.join(root, `${key}.json`);
}

export function localAgentStateDir(localStateRoot, agentId) {
  const agentsRoot = path.resolve(localStateRoot, "agents");
  const digest = createHash("sha256").update(String(agentId)).digest("hex");
  const directory = path.resolve(agentsRoot, `agent-${digest}`);
  if (path.dirname(directory) !== agentsRoot || !/^agent-[a-f0-9]{64}$/.test(path.basename(directory))) {
    throw new Error("refusing unsafe SDK agent state path");
  }
  return directory;
}

function readMarker(file) {
  try {
    const value = JSON.parse(readFileSync(file, "utf8"));
    if (value?.version !== RECORD_VERSION || typeof value.agentId !== "string"
        || typeof value.scope !== "string" || !Number.isSafeInteger(value.quarantinedAt)) return null;
    return value;
  } catch (error) {
    if (error?.code === "ENOENT") return null;
    return null;
  }
}

async function readMarkerAsync(file) {
  try {
    const value = JSON.parse(await readFile(file, "utf8"));
    if (value?.version !== RECORD_VERSION || typeof value.agentId !== "string"
        || typeof value.scope !== "string" || !Number.isSafeInteger(value.quarantinedAt)) return null;
    return value;
  } catch (error) {
    if (error?.code === "ENOENT") return null;
    return null;
  }
}

async function listMarkers(root) {
  try {
    const entries = await readdir(root, { withFileTypes: true });
    const records = [];
    for (const entry of entries) {
      if (!entry.isFile() || !/^[a-f0-9]{64}\.json$/.test(entry.name)) continue;
      const file = path.join(root, entry.name);
      const marker = await readMarkerAsync(file);
      if (marker) records.push({ file, marker });
    }
    return records;
  } catch (error) {
    if (error?.code === "ENOENT") return [];
    throw error;
  }
}

async function removeLocalAgentState(localStateRoot, agentId) {
  if (!localStateRoot) return false;
  const directory = localAgentStateDir(localStateRoot, agentId);
  try {
    await lstat(directory);
  } catch (error) {
    if (error?.code === "ENOENT") return false;
    throw error;
  }
  await rm(directory, { recursive: true, force: true });
  return true;
}

function writeMarker(file, record) {
  mkdirSync(path.dirname(file), { recursive: true, mode: 0o700 });
  const temp = `${file}.${process.pid}.${randomUUID()}.tmp`;
  const bytes = Buffer.from(`${JSON.stringify(record)}\n`, "utf8");
  let fd = null;
  try {
    fd = openSync(temp, "wx", 0o600);
    let offset = 0;
    while (offset < bytes.length) offset += writeSync(fd, bytes, offset, bytes.length - offset);
    fsyncSync(fd);
    closeSync(fd);
    fd = null;
    renameSync(temp, file);
  } finally {
    if (fd !== null) try { closeSync(fd); } catch {}
    try { rmSync(temp, { force: true }); } catch {}
  }
}

function agentModifiedAt(agent) {
  const value = Number(agent?.lastModified ?? agent?.updatedAt ?? agent?.createdAt ?? 0);
  return Number.isFinite(value) && value > 0 ? value : 0;
}

function agentIsArchived(agent) {
  return agent?.archived === true || agent?.status === "ARCHIVED";
}

function isNotFound(error) {
  return /not found|no agents matched/i.test((error && error.message) || String(error));
}

async function listAgentsBounded(platform, maxScan, pageSize = 200) {
  const agents = [];
  let cursor;
  while (agents.length < maxScan) {
    const page = await platform.listAgents({ limit: Math.min(pageSize, maxScan - agents.length), ...(cursor ? { cursor } : {}) });
    const items = Array.isArray(page?.items) ? page.items : [];
    agents.push(...items);
    cursor = page?.nextCursor;
    if (!cursor || items.length === 0) break;
  }
  return agents;
}

// Two-phase SDK-agent collection. An unreferenced idle agent is archived and
// recorded first. It is deleted only after the quarantine TTL and a fresh root
// check. If it becomes referenced meanwhile, it is unarchived automatically.
export async function collectSdkAgents({
  platform,
  scope,
  quarantineRoot,
  localStateRoot,
  protectedAgentIds,
  refreshProtectedAgentIds = () => protectedAgentIds,
  now = () => Date.now(),
  minIdleMs,
  quarantineMs,
  maxScan,
  maxMutations,
}) {
  const stats = {
    scanned: 0,
    protected: 0,
    quarantined: 0,
    deleted: 0,
    localDeleted: 0,
    markersCleared: 0,
    restored: 0,
    skipped: 0,
  };
  const agents = await listAgentsBounded(platform, maxScan);
  stats.scanned = agents.length;
  let mutations = 0;

  // Cursor may stop listing an archived/deleted agent before its SDK checkpoint
  // directory is removed. The durable marker is the only safe authority for
  // locating those orphans, so sweep eligible markers before creating new ones.
  const listedAgentIds = new Set(agents.map((agent) => String(agent?.agentId || "")).filter(Boolean));
  for (const { file, marker } of await listMarkers(quarantineRoot)) {
    if (mutations >= maxMutations) break;
    if (marker.scope !== String(scope) || listedAgentIds.has(marker.agentId)
        || now() - marker.quarantinedAt < quarantineMs) continue;
    let roots = await refreshProtectedAgentIds();
    if (roots.has(marker.agentId)) { stats.protected++; continue; }
    let current = null;
    try {
      current = await platform.getAgent(marker.agentId);
    } catch (error) {
      if (!isNotFound(error)) { stats.skipped++; continue; }
    }
    if (current && !agentIsArchived(current)) {
      rmSync(file, { force: true });
      stats.restored++;
      mutations++;
      continue;
    }
    roots = await refreshProtectedAgentIds();
    if (roots.has(marker.agentId)) { stats.protected++; continue; }
    try {
      if (current) {
        await platform.deleteAgent(marker.agentId);
        stats.deleted++;
      }
      if (await removeLocalAgentState(localStateRoot, marker.agentId)) stats.localDeleted++;
      rmSync(file, { force: true });
      stats.markersCleared++;
      mutations++;
    } catch (error) {
      stats.skipped++;
    }
  }

  for (const agent of agents) {
    const agentId = String(agent?.agentId || "");
    if (!agentId) { stats.skipped++; continue; }
    const file = markerPath(quarantineRoot, scope, agentId);
    const marker = readMarker(file);
    let roots = protectedAgentIds;
    if (roots.has(agentId)) {
      stats.protected++;
      if (marker && mutations < maxMutations) {
        try {
          if (agentIsArchived(agent)) await platform.unarchiveAgent(agentId);
          rmSync(file, { force: true });
          stats.restored++;
          mutations++;
        } catch { stats.skipped++; }
      }
      continue;
    }
    if (!marker) {
      const modifiedAt = agentModifiedAt(agent);
      if (!modifiedAt || now() - modifiedAt < minIdleMs || mutations >= maxMutations) continue;
      roots = await refreshProtectedAgentIds();
      if (roots.has(agentId)) { stats.protected++; continue; }
      try {
        await platform.archiveAgent(agentId);
        writeMarker(file, { version: RECORD_VERSION, scope: String(scope), agentId, quarantinedAt: now() });
        stats.quarantined++;
        mutations++;
      } catch (error) {
        if (!isNotFound(error)) stats.skipped++;
      }
      continue;
    }
    // Another process/operator may have resumed or explicitly unarchived the
    // agent during quarantine. That is positive liveness evidence even when
    // this process has no corresponding in-memory root: cancel this GC claim
    // and require a brand-new idle interval before reconsidering it.
    if (!agentIsArchived(agent)) {
      rmSync(file, { force: true });
      stats.restored++;
      continue;
    }
    if (now() - marker.quarantinedAt < quarantineMs || mutations >= maxMutations) continue;
    roots = await refreshProtectedAgentIds();
    if (roots.has(agentId)) { stats.protected++; continue; }
    try {
      const current = await platform.getAgent(agentId);
      if (!agentIsArchived(current)) {
        rmSync(file, { force: true });
        stats.restored++;
        continue;
      }
      roots = await refreshProtectedAgentIds();
      if (roots.has(agentId)) { stats.protected++; continue; }
      await platform.deleteAgent(agentId);
      if (await removeLocalAgentState(localStateRoot, agentId)) stats.localDeleted++;
      rmSync(file, { force: true });
      stats.deleted++;
      mutations++;
    } catch (error) {
      if (!isNotFound(error)) {
        stats.skipped++;
        continue;
      }
      roots = await refreshProtectedAgentIds();
      if (roots.has(agentId)) { stats.protected++; continue; }
      try {
        if (await removeLocalAgentState(localStateRoot, agentId)) stats.localDeleted++;
        rmSync(file, { force: true });
        stats.markersCleared++;
        mutations++;
      } catch { stats.skipped++; }
    }
  }
  return stats;
}

export { markerPath, readMarker };
