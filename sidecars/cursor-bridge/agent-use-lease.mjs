import { createHash, randomUUID } from "node:crypto";
import { mkdir, open, readdir, readFile, rm, stat, writeFile, rename, utimes } from "node:fs/promises";
import path from "node:path";

export class AgentLeaseBusyError extends Error {
  constructor(message = "agent lifecycle lease is busy") {
    super(message);
    this.name = "AgentLeaseBusyError";
    this.code = "agent_lifecycle_busy";
    this.status = 503;
    this.retryable = true;
  }
}

const delay = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

export class DurableAgentLeaseManager {
  constructor(root, {
    now = () => Date.now(),
    useLeaseMs = 10 * 60_000,
    useLeaseStaleMs = 60 * 60_000,
    heartbeatMs = 30_000,
    lockStaleMs = 15 * 60_000,
    lockWaitMs = 5_000,
  } = {}) {
    this.root = path.resolve(root);
    this.now = now;
    this.useLeaseMs = useLeaseMs;
    this.useLeaseStaleMs = useLeaseStaleMs;
    this.heartbeatMs = heartbeatMs;
    this.lockStaleMs = lockStaleMs;
    this.lockWaitMs = lockWaitMs;
  }

  key(scope, agentId) {
    return createHash("sha256").update(String(scope)).update("\0").update(String(agentId)).digest("hex");
  }

  paths(scope, agentId) {
    const key = this.key(scope, agentId);
    return {
      lock: path.join(this.root, `${key}.mutation-lock`),
      uses: path.join(this.root, `${key}.uses`),
    };
  }

  async acquireLock(lock) {
    await mkdir(this.root, { recursive: true, mode: 0o700 });
    const deadline = this.now() + this.lockWaitMs;
    while (true) {
      try {
        await mkdir(lock, { mode: 0o700 });
        const owner = randomUUID();
        const ownerFile = path.join(lock, "owner");
        await writeFile(ownerFile, `${owner}\n`, { mode: 0o600, flag: "wx" });
        const stillOwner = async () => {
          try { return (await readFile(ownerFile, "utf8")).trim() === owner; }
          catch { return false; }
        };
        const heartbeat = setInterval(() => {
          void (async () => {
            if (!await stillOwner()) return;
            const at = new Date(this.now());
            await utimes(lock, at, at);
          })().catch(() => {});
        }, Math.max(1_000, Math.floor(this.lockStaleMs / 3)));
        heartbeat.unref?.();
        return async () => {
          clearInterval(heartbeat);
          if (await stillOwner()) {
            const at = new Date(this.now());
            await utimes(lock, at, at).catch(() => {});
          }
          if (!await stillOwner()) return false;
          const retired = `${lock}.released.${owner}`;
          try { await rename(lock, retired); }
          catch (error) {
            if (error?.code === "ENOENT") return false;
            throw error;
          }
          if ((await readFile(path.join(retired, "owner"), "utf8").catch(() => "")).trim() !== owner) {
            try { await rename(retired, lock); } catch {}
            return false;
          }
          await rm(retired, { recursive: true, force: true });
          return true;
        };
      } catch (error) {
        if (error?.code !== "EEXIST") throw error;
        try {
          const info = await stat(lock);
          if (this.now() - info.mtimeMs >= this.lockStaleMs) {
            const observedOwner = (await readFile(path.join(lock, "owner"), "utf8").catch(() => "")).trim();
            const retired = `${lock}.stale.${randomUUID()}`;
            try { await rename(lock, retired); }
            catch (renameError) {
              if (renameError?.code === "ENOENT" || renameError?.code === "EEXIST") continue;
              throw renameError;
            }
            const retiredOwner = (await readFile(path.join(retired, "owner"), "utf8").catch(() => "")).trim();
            const retiredInfo = await stat(retired).catch(() => null);
            if (!observedOwner || retiredOwner !== observedOwner
                || !retiredInfo || retiredInfo.mtimeMs !== info.mtimeMs) {
              try { await rename(retired, lock); } catch {}
              continue;
            }
            await rm(retired, { recursive: true, force: true });
            continue;
          }
        } catch (statError) {
          if (statError?.code === "ENOENT") continue;
          throw statError;
        }
        if (this.now() >= deadline) throw new AgentLeaseBusyError();
        await delay(20);
      }
    }
  }

  async activeUseLeases(uses) {
    let names;
    try { names = await readdir(uses); }
    catch (error) {
      if (error?.code === "ENOENT") return [];
      throw error;
    }
    const active = [];
    for (const name of names) {
      if (!/^[a-f0-9-]+\.json$/.test(name)) continue;
      const file = path.join(uses, name);
      let record;
      try { record = JSON.parse(await readFile(file, "utf8")); }
      catch (error) {
        if (error?.code === "ENOENT") continue;
        throw new Error(`cannot decode agent use lease ${name}: ${error.message}`);
      }
      if (!record || record.version !== 1 || !Number.isSafeInteger(record.expiresAt)) {
        throw new Error(`agent use lease ${name} is malformed`);
      }
      if (record.expiresAt + this.useLeaseStaleMs <= this.now()) await rm(file, { force: true });
      else active.push(record);
    }
    return active;
  }

  async writeUseLease(file, record) {
    const directory = path.dirname(file);
    await mkdir(directory, { recursive: true, mode: 0o700 });
    const temp = `${file}.${process.pid}.${randomUUID()}.tmp`;
    let handle = null;
    try {
      handle = await open(temp, "wx", 0o600);
      await handle.writeFile(`${JSON.stringify(record)}\n`);
      await handle.sync();
      await handle.close();
      handle = null;
      await rename(temp, file);
      const directoryHandle = await open(directory, "r");
      try { await directoryHandle.sync(); } finally { await directoryHandle.close(); }
    } finally {
      if (handle) await handle.close().catch(() => {});
      await rm(temp, { force: true }).catch(() => {});
    }
  }

  async acquireUse(scope, agentId, { beforeAcquire = async () => {}, onLeaseLost = async () => {} } = {}) {
    const { lock, uses } = this.paths(scope, agentId);
    const releaseLock = await this.acquireLock(lock);
    const holder = randomUUID();
    const file = path.join(uses, `${holder}.json`);
    let released = false;
    let renewalError = null;
    let consecutiveFailures = 0;
    let lostNotified = false;
    const acquiredAt = this.now();
    let renewal = Promise.resolve();
    const renew = async () => {
      if (released) return;
      await this.writeUseLease(file, {
        version: 1,
        holder,
        pid: process.pid,
        acquiredAt,
        expiresAt: this.now() + this.useLeaseMs,
      });
      renewalError = null;
      consecutiveFailures = 0;
    };
    try {
      await this.activeUseLeases(uses);
      await beforeAcquire();
      await renew();
    } finally {
      await releaseLock();
    }
    const timer = setInterval(() => {
      renewal = renewal.then(renew).catch(async (error) => {
        renewalError = error;
        consecutiveFailures++;
        // P3.2 fail-closed: once renewals have been failing long enough that the lease file
        // will expire before the next successful heartbeat, the turn must not continue
        // silently while GC could legally collect its agent. Notify ONCE; the bridge aborts
        // the turn with a typed retryable error and refreshes durable roots (alias touch) so
        // protection holds even if the lease file lapses.
        if (!lostNotified
            && consecutiveFailures * this.heartbeatMs >= Math.max(this.heartbeatMs, Math.floor(this.useLeaseMs / 2))) {
          lostNotified = true;
          try {
            await onLeaseLost({
              scope: String(scope), agentId: String(agentId), holder,
              consecutiveFailures, error,
            });
          } catch { /* observer failure never breaks the heartbeat loop */ }
        }
      });
    }, this.heartbeatMs);
    timer.unref?.();
    return {
      release: async () => {
        if (released) return false;
        released = true;
        clearInterval(timer);
        await renewal;
        await rm(file, { force: true });
        if (renewalError) throw renewalError;
        return true;
      },
    };
  }

  async withMutation({ scope, agentId }, mutate) {
    return this.withLock({ scope, agentId }, async ({ uses }) => {
      if ((await this.activeUseLeases(uses)).length > 0) throw new AgentLeaseBusyError("agent is in active use");
      return mutate();
    });
  }

  async withLock({ scope, agentId }, mutate) {
    const { lock, uses } = this.paths(scope, agentId);
    const releaseLock = await this.acquireLock(lock);
    try {
      return await mutate({ uses });
    } finally {
      await releaseLock();
    }
  }

  async withLeader(name, mutate) {
    const lock = path.join(this.root, `${this.key("leader", name)}.leader-lock`);
    const releaseLock = await this.acquireLock(lock);
    try { return await mutate(); }
    finally { await releaseLock(); }
  }
}
