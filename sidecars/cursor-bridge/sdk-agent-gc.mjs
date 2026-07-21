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
const GC_SAFETY_VERSION = 2;

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
        || typeof value.scope !== "string" || !Number.isSafeInteger(value.quarantinedAt)
        || (value.safetyVersion !== undefined && !Number.isSafeInteger(value.safetyVersion))
        || (value.status !== undefined && !["quarantined", "deleted"].includes(value.status))
        || (value.status === "deleted" && !Number.isSafeInteger(value.deletedAt))) {
      throw new Error(`invalid SDK agent GC marker: ${path.basename(file)}`);
    }
    return value;
  } catch (error) {
    if (error?.code === "ENOENT") return null;
    throw error;
  }
}

async function readMarkerAsync(file) {
  try {
    const value = JSON.parse(await readFile(file, "utf8"));
    if (value?.version !== RECORD_VERSION || typeof value.agentId !== "string"
        || typeof value.scope !== "string" || !Number.isSafeInteger(value.quarantinedAt)
        || (value.safetyVersion !== undefined && !Number.isSafeInteger(value.safetyVersion))
        || (value.status !== undefined && !["quarantined", "deleted"].includes(value.status))
        || (value.status === "deleted" && !Number.isSafeInteger(value.deletedAt))) {
      throw new Error(`invalid SDK agent GC marker: ${path.basename(file)}`);
    }
    return value;
  } catch (error) {
    if (error?.code === "ENOENT") return null;
    throw error;
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

function agentLifecycleRank(agent) {
  const status = String(agent?.status || "").trim().toUpperCase();
  if (agentIsArchived(agent) || ["ERROR", "FAILED", "COMPLETED", "TERMINAL"].includes(status)) return 0;
  if (["IDLE", "STOPPED", "CANCELLED", "CANCELED"].includes(status)) return 1;
  if (["RUNNING", "ACTIVE"].includes(status)) return 3;
  return 2;
}

function agentIdleThreshold(agent, { minIdleMs, terminalIdleMs, runningIdleMs }) {
  const rank = agentLifecycleRank(agent);
  if (rank === 0) return terminalIdleMs;
  if (rank === 3) return runningIdleMs;
  return minIdleMs;
}

export function compareSdkAgentsForGC(a, b) {
  const rank = agentLifecycleRank(a) - agentLifecycleRank(b);
  if (rank !== 0) return rank;
  const age = agentModifiedAt(a) - agentModifiedAt(b);
  if (age !== 0) return age;
  return String(a?.agentId || "").localeCompare(String(b?.agentId || ""));
}

export function gcPressureDecision({ usedBytes, highWaterBytes, lowWaterBytes, active = false } = {}) {
  const used = Number(usedBytes);
  const high = Number(highWaterBytes);
  const low = Number(lowWaterBytes);
  if (![used, high, low].every(Number.isFinite) || low < 0 || high <= low) {
    throw new Error("invalid SDK agent GC pressure watermarks");
  }
  return { active: active ? used > low : used >= high, usedBytes: used, highWaterBytes: high, lowWaterBytes: low };
}

function normalizeRootSnapshot(value) {
  if (value instanceof Set) return { roots: value, complete: true, generation: null };
  if (!value || value.complete !== true) {
    return { roots: new Set(), complete: false, generation: value?.generation ?? null };
  }
  try {
    return {
      roots: value.roots instanceof Set ? value.roots : new Set(value.roots || []),
      complete: true,
      generation: value.generation ?? null,
    };
  } catch {
    return { roots: new Set(), complete: false, generation: value?.generation ?? null };
  }
}

function isNotFound(error) {
  return /not found|no agents matched/i.test((error && error.message) || String(error));
}

// Request-path integration helper. A bridge that is about to resume an agent
// calls this under the same shared per-agent lease used by collectSdkAgents.
// The marker is removed only after the remote agent is proven usable again;
// a missing remote agent keeps the marker/tombstone intact for the caller's
// explicit reseed-or-410 decision.
export async function cancelSdkAgentQuarantine({
  platform,
  scope,
  quarantineRoot,
  agentId,
  withAgentMutationLease = async (_context, restore) => restore(),
}) {
  return withAgentMutationLease({ scope: String(scope), agentId: String(agentId), operation: "cancel-quarantine" }, async () => {
    const file = markerPath(quarantineRoot, scope, agentId);
    const marker = readMarker(file);
    if (!marker) return { restored: false, missing: false };
    if (marker.scope !== String(scope) || marker.agentId !== String(agentId)) {
      throw new Error("SDK agent GC marker does not match its quarantine key");
    }
    let current;
    try {
      current = await platform.getAgent(agentId);
    } catch (error) {
      if (isNotFound(error)) return { restored: false, missing: true };
      throw error;
    }
    if (agentIsArchived(current)) await platform.unarchiveAgent(agentId);
    rmSync(file, { force: true });
    return { restored: true, missing: false };
  });
}

async function listAgentsBounded(platform, maxScan, pageSize = 200, startCursor = "") {
  const agents = [];
  let cursor = startCursor || undefined;
  while (agents.length < maxScan) {
    const page = await platform.listAgents({ limit: Math.min(pageSize, maxScan - agents.length), ...(cursor ? { cursor } : {}) });
    const items = Array.isArray(page?.items) ? page.items : [];
    agents.push(...items);
    cursor = page?.nextCursor;
    if (!cursor || items.length === 0) break;
  }
  return { agents, nextCursor: cursor || "" };
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
  terminalIdleMs = Math.min(minIdleMs, 60 * 60 * 1000),
  runningIdleMs = Math.max(minIdleMs, 7 * 24 * 60 * 60 * 1000),
  quarantineMs,
  maxScan,
  maxMutations,
  tombstoneMs = Number.POSITIVE_INFINITY,
  scanCursor = "",
  pressure = null,
  // Bridge integration seam: this callback must serialize GC mutation with
  // request-side use-lease acquisition for the exact agent across processes.
  withAgentMutationLease = async (_context, mutate) => mutate(),
  // Test/observability seam invoked while the mutation lease is held. A fresh
  // root snapshot is taken after it, closing races injected at this boundary.
  beforeMutation = async () => {},
  // Observability seam: invoked for every lifecycle mutation (quarantined,
  // deleted, local-deleted, restored) with a content-free descriptor. The bridge
  // maps these onto lifecycleEvent lines — the per-agent attribution trail whose
  // absence made the 2026-07-20 incident unattributable at the resume point.
  onEvent = () => {},
  // Optional shared-generation validator. The bridge can reject a mutation if
  // its durable lease generation changed after the final root snapshot.
  validateMutationGeneration = async () => true,
  // P6.4: tombstone compaction authority. A deleted marker is retained until this
  // returns true — the bridge proves no durable handle (alias/response route) can
  // still name the id and the tombstone has aged past retention.
  tombstoneCompactable = () => false,
  // P6.5: best-effort archive-compress the local checkpoint before physical deletion
  // (forensic/rollback insurance; never blocks deletion).
  archiveBeforeDelete = async () => {},
}) {
  const stats = {
    scanned: 0,
    protected: 0,
    quarantined: 0,
    requarantined: 0,
    deleted: 0,
    localDeleted: 0,
    markersCleared: 0,
    restored: 0,
    skipped: 0,
    fenced: 0,
    incompleteRootSnapshots: 0,
    aborted: false,
    nextScanCursor: "",
    pressureActive: pressure ? gcPressureDecision(pressure).active : null,
  };
  const initialRoots = normalizeRootSnapshot(protectedAgentIds);
  if (!initialRoots.complete) {
    stats.incompleteRootSnapshots++;
    stats.aborted = true;
    return stats;
  }
  const destructiveAllowed = !pressure || stats.pressureActive;
  const listing = await listAgentsBounded(platform, maxScan, 200, scanCursor);
  const agents = listing.agents.sort(compareSdkAgentsForGC);
  stats.scanned = agents.length;
  stats.nextScanCursor = listing.nextCursor;
  let mutations = 0;

  const refreshRoots = async () => {
    let snapshot;
    try {
      snapshot = normalizeRootSnapshot(await refreshProtectedAgentIds());
    } catch {
      snapshot = { roots: new Set(), complete: false, generation: null };
    }
    if (!snapshot.complete) {
      stats.incompleteRootSnapshots++;
      stats.aborted = true;
    }
    return snapshot;
  };
  const mutateIfUnprotected = async (agentId, operation, mutate) => {
    if (!destructiveAllowed || stats.aborted) return "disabled";
    return withAgentMutationLease({ scope: String(scope), agentId, operation }, async () => {
      let snapshot = await refreshRoots();
      if (!snapshot.complete) return "incomplete";
      if (snapshot.roots.has(agentId)) { stats.protected++; return "protected"; }
      await beforeMutation({ scope: String(scope), agentId, operation, generation: snapshot.generation });
      snapshot = await refreshRoots();
      if (!snapshot.complete) return "incomplete";
      if (snapshot.roots.has(agentId)) { stats.protected++; return "protected"; }
      if (!await validateMutationGeneration({
        scope: String(scope), agentId, operation, generation: snapshot.generation,
      })) {
        stats.fenced++;
        return "fenced";
      }
      if (await mutate() === false) return "precondition";
      return "mutated";
    });
  };
  const restoreIfActive = async (agentId, file, operation) => {
    try {
      return await withAgentMutationLease({ scope: String(scope), agentId, operation }, async () => {
        const latestMarker = readMarker(file);
        if (!latestMarker || latestMarker.status === "deleted") return false;
        let latest;
        try { latest = await platform.getAgent(agentId); }
        catch (error) { if (isNotFound(error)) return false; throw error; }
        if (agentIsArchived(latest)) return false;
        rmSync(file, { force: true });
        stats.restored++;
        onEvent({ operation: "restored", agentId: String(agentId), scope: String(scope) });
        return true;
      });
    } catch {
      stats.skipped++;
      return false;
    }
  };

  // Cursor may stop listing an archived/deleted agent before its SDK checkpoint
  // directory is removed. The durable marker is the only safe authority for
  // locating those orphans, so sweep eligible markers before creating new ones.
  const listedAgentIds = new Set(agents.map((agent) => String(agent?.agentId || "")).filter(Boolean));
  // Read and validate the entire marker set before the first mutation. Reusing
  // this immutable census also prevents a late malformed marker from turning a
  // partially completed batch into an accidental fail-open run.
  const markerRecords = (await listMarkers(quarantineRoot)).sort((a, b) =>
    a.marker.quarantinedAt - b.marker.quarantinedAt
      || a.marker.agentId.localeCompare(b.marker.agentId));
  const markerByAgentId = new Map(markerRecords
    .filter((entry) => entry.marker.scope === String(scope))
    .map((entry) => [entry.marker.agentId, entry]));
  for (const { file, marker } of markerRecords) {
    if (mutations >= maxMutations || stats.aborted) break;
    // Deleted markers are continuity tombstones, not a cache. An alias or a
    // previously issued response id can outlive any local age heuristic; if we
    // discard the marker, resumeAgent(not-found) can silently create an empty
    // agent. Retain it until the compaction authority proves no durable handle
    // can name this id (P6.4: age + alias census, supplied by the bridge).
    if (marker.status === "deleted") {
      if (tombstoneCompactable(marker)) {
        try { rmSync(file, { force: true }); stats.markersCleared++; } catch {}
      }
      continue;
    }
    // Markers created by the original 60-second quarantine policy must never
    // inherit their old timestamp when the safe collector is enabled. Give
    // every legacy claim a fresh full quarantine interval under the current
    // cross-process fences before it can authorize deletion.
    if (marker.scope === String(scope) && marker.safetyVersion !== GC_SAFETY_VERSION) {
      try {
        const outcome = await mutateIfUnprotected(marker.agentId, "requarantine-legacy", async () => {
          const latestMarker = readMarker(file);
          if (!latestMarker || latestMarker.status === "deleted"
              || latestMarker.safetyVersion === GC_SAFETY_VERSION) return false;
          const refreshed = {
            ...latestMarker,
            safetyVersion: GC_SAFETY_VERSION,
            quarantinedAt: now(),
            status: "quarantined",
          };
          writeMarker(file, refreshed);
          markerByAgentId.set(marker.agentId, { file, marker: refreshed });
          stats.requarantined++;
          onEvent({ operation: "requarantined", agentId: String(marker.agentId), scope: String(scope) });
        });
        if (outcome === "mutated") mutations++;
      } catch { stats.skipped++; }
      continue;
    }
    if (marker.scope !== String(scope) || listedAgentIds.has(marker.agentId)
        || now() - marker.quarantinedAt < quarantineMs) continue;
    let current = null;
    try {
      current = await platform.getAgent(marker.agentId);
    } catch (error) {
      if (!isNotFound(error)) { stats.skipped++; continue; }
    }
    if (current && !agentIsArchived(current)) {
      if (await restoreIfActive(marker.agentId, file, "restore-unlisted")) mutations++;
      continue;
    }
    try {
      const outcome = await mutateIfUnprotected(marker.agentId, "delete-unlisted", async () => {
        const latestMarker = readMarker(file);
        if (!latestMarker || latestMarker.status === "deleted") return false;
        let latest = null;
        try { latest = await platform.getAgent(marker.agentId); }
        catch (error) { if (!isNotFound(error)) throw error; }
        if (latest && !agentIsArchived(latest)) {
          rmSync(file, { force: true });
          stats.restored++;
          onEvent({ operation: "restored", agentId: String(marker.agentId), scope: String(scope) });
          return true;
        }
        if (latest) {
          await platform.deleteAgent(marker.agentId);
          stats.deleted++;
        }
        try { await archiveBeforeDelete({ scope: String(scope), agentId: String(marker.agentId) }); } catch {}
        if (await removeLocalAgentState(localStateRoot, marker.agentId)) stats.localDeleted++;
        writeMarker(file, {
          version: RECORD_VERSION,
          safetyVersion: GC_SAFETY_VERSION,
          scope: String(scope),
          agentId: marker.agentId,
          quarantinedAt: marker.quarantinedAt,
          status: "deleted",
          deletedAt: now(),
        });
        onEvent({ operation: "deleted", agentId: String(marker.agentId), scope: String(scope), unlisted: true });
      });
      if (outcome === "mutated") mutations++;
    } catch (error) {
      stats.skipped++;
    }
  }

  for (const agent of agents) {
    const agentId = String(agent?.agentId || "");
    if (!agentId) { stats.skipped++; continue; }
    const file = markerPath(quarantineRoot, scope, agentId);
    const marker = markerByAgentId.get(agentId)?.marker || null;
    if (marker?.status === "deleted") continue;
    if (initialRoots.roots.has(agentId)) {
      stats.protected++;
      if (marker && mutations < maxMutations) {
        try {
          const restored = await withAgentMutationLease(
            { scope: String(scope), agentId, operation: "restore-protected" },
            async () => {
              const latestMarker = readMarker(file);
              if (!latestMarker || latestMarker.status === "deleted") return false;
              let latest;
              try { latest = await platform.getAgent(agentId); }
              catch (error) { if (isNotFound(error)) return false; throw error; }
              if (agentIsArchived(latest)) await platform.unarchiveAgent(agentId);
              rmSync(file, { force: true });
              stats.restored++;
              onEvent({ operation: "restored", agentId: String(agentId), scope: String(scope) });
              return true;
            },
          );
          if (restored) mutations++;
        } catch { stats.skipped++; }
      }
      continue;
    }
    if (!marker) {
      const modifiedAt = agentModifiedAt(agent);
      const idleThreshold = agentIdleThreshold(agent, { minIdleMs, terminalIdleMs, runningIdleMs });
      if (!modifiedAt || now() - modifiedAt < idleThreshold || mutations >= maxMutations) continue;
      try {
        const outcome = await mutateIfUnprotected(agentId, "archive", async () => {
          if (readMarker(file)) return false;
          let latest;
          try { latest = await platform.getAgent(agentId); }
          catch (error) { if (isNotFound(error)) return false; throw error; }
          const latestModifiedAt = agentModifiedAt(latest);
          const latestIdleThreshold = agentIdleThreshold(latest, { minIdleMs, terminalIdleMs, runningIdleMs });
          if (!latestModifiedAt || now() - latestModifiedAt < latestIdleThreshold) return false;
          await platform.archiveAgent(agentId);
          writeMarker(file, {
            version: RECORD_VERSION, scope: String(scope), agentId,
            safetyVersion: GC_SAFETY_VERSION,
            quarantinedAt: now(), status: "quarantined",
          });
          stats.quarantined++;
          onEvent({ operation: "quarantined", agentId: String(agentId), scope: String(scope) });
        });
        if (outcome === "mutated") mutations++;
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
      if (await restoreIfActive(agentId, file, "restore-listed")) mutations++;
      continue;
    }
    if (now() - marker.quarantinedAt < quarantineMs || mutations >= maxMutations) continue;
    try {
      const current = await platform.getAgent(agentId);
      if (!agentIsArchived(current)) {
        if (await restoreIfActive(agentId, file, "restore-delete-preflight")) mutations++;
        continue;
      }
      const outcome = await mutateIfUnprotected(agentId, "delete", async () => {
        const latestMarker = readMarker(file);
        if (!latestMarker || latestMarker.status === "deleted") return false;
        let latest = null;
        try { latest = await platform.getAgent(agentId); }
        catch (error) { if (!isNotFound(error)) throw error; }
        if (latest && !agentIsArchived(latest)) {
          rmSync(file, { force: true });
          stats.restored++;
          onEvent({ operation: "restored", agentId: String(agentId), scope: String(scope) });
          return true;
        }
        if (latest) await platform.deleteAgent(agentId);
        try { await archiveBeforeDelete({ scope: String(scope), agentId: String(agentId) }); } catch {}
        if (await removeLocalAgentState(localStateRoot, agentId)) stats.localDeleted++;
        writeMarker(file, {
          version: RECORD_VERSION,
          safetyVersion: GC_SAFETY_VERSION,
          scope: String(scope),
          agentId,
          quarantinedAt: latestMarker.quarantinedAt,
          status: "deleted",
          deletedAt: now(),
        });
        onEvent({ operation: "deleted", agentId: String(agentId), scope: String(scope) });
        if (latest) stats.deleted++;
      });
      if (outcome === "mutated") mutations++;
    } catch (error) {
      if (!isNotFound(error)) {
        stats.skipped++;
        continue;
      }
      try {
        const outcome = await mutateIfUnprotected(agentId, "delete-local-orphan", async () => {
          const latestMarker = readMarker(file);
          if (!latestMarker || latestMarker.status === "deleted") return false;
          try {
            const latest = await platform.getAgent(agentId);
            if (!agentIsArchived(latest)) {
              rmSync(file, { force: true });
              stats.restored++;
              onEvent({ operation: "restored", agentId: String(agentId), scope: String(scope) });
              return true;
            }
            return false;
          } catch (error) {
            if (!isNotFound(error)) throw error;
          }
          try { await archiveBeforeDelete({ scope: String(scope), agentId: String(agentId) }); } catch {}
          if (await removeLocalAgentState(localStateRoot, agentId)) stats.localDeleted++;
          writeMarker(file, {
            version: RECORD_VERSION,
            safetyVersion: GC_SAFETY_VERSION,
            scope: String(scope),
            agentId,
            quarantinedAt: latestMarker.quarantinedAt,
            status: "deleted",
            deletedAt: now(),
          });
          onEvent({ operation: "local-deleted", agentId: String(agentId), scope: String(scope) });
        });
        if (outcome === "mutated") mutations++;
      } catch { stats.skipped++; }
    }
  }
  return stats;
}

export { markerPath, readMarker, writeMarker };
