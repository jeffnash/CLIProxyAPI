import {
  closeSync,
  existsSync,
  fsyncSync,
  linkSync,
  mkdirSync,
  openSync,
  readFileSync,
  readdirSync,
  renameSync,
  rmSync,
  statSync,
  writeSync,
} from "node:fs";
import { createHash, randomUUID } from "node:crypto";
import path from "node:path";

export class DurableCASConflict extends Error {
  constructor(message) {
    super(message);
    this.name = "DurableCASConflict";
  }
}

export function canonicalRecordDigest(record) {
  return createHash("sha256").update(JSON.stringify(record)).digest("hex");
}

function fsyncDirectory(dir) {
  const fd = openSync(dir, "r");
  try { fsyncSync(fd); } finally { closeSync(fd); }
}

function writeExclusive(file, bytes) {
  const fd = openSync(file, "wx", 0o600);
  try {
    let offset = 0;
    while (offset < bytes.length) offset += writeSync(fd, bytes, offset, bytes.length - offset);
    fsyncSync(fd);
  } finally {
    closeSync(fd);
  }
}

export class DurableJSONCAS {
  read(file) {
    try {
      const record = JSON.parse(readFileSync(file, "utf8"));
      return { record, revision: Number.isSafeInteger(record.revision) ? record.revision : 0, digest: canonicalRecordDigest(record) };
    } catch (error) {
      if (error && error.code === "ENOENT") return { record: null, revision: 0, digest: "" };
      throw error;
    }
  }

  readRecover(file) {
    let state = this.read(file);
    for (let attempts = 0; attempts < 64; attempts++) {
      const claimFile = this.claimPath(file, state.revision + 1);
      if (!existsSync(claimFile)) return state;
      state = this.recover(file, state.revision + 1);
    }
    throw new Error("durable CAS recovery exceeded the revision bound");
  }

  claimPath(file, revision) {
    return `${file}.cas.r${revision}.claim`;
  }

  recoverClaim(file, claim) {
    const current = this.read(file);
    if (current.revision > claim.nextRevision) return current;
    if (current.revision === claim.nextRevision) {
      if (current.digest !== claim.nextDigest) throw new Error("durable CAS canonical digest conflicts with its immutable claim");
      return current;
    }
    if (current.revision !== claim.expectedRevision || current.digest !== claim.expectedDigest) {
      throw new DurableCASConflict("durable CAS expected state changed before claimed commit recovery");
    }
    const candidate = path.join(path.dirname(file), claim.candidate);
    let next;
    try {
      next = JSON.parse(readFileSync(candidate, "utf8"));
    } catch (error) {
      if (!error || error.code !== "ENOENT") throw error;
      const helped = this.read(file);
      if (helped.revision !== claim.nextRevision || helped.digest !== claim.nextDigest) throw error;
      return helped;
    }
    if (canonicalRecordDigest(next) !== claim.nextDigest || next.revision !== claim.nextRevision) {
      throw new Error("durable CAS claimed candidate digest is invalid");
    }
    try {
      renameSync(candidate, file);
      fsyncDirectory(path.dirname(file));
    } catch (error) {
      if (!error || error.code !== "ENOENT") throw error;
      const helped = this.read(file);
      if (helped.revision !== claim.nextRevision || helped.digest !== claim.nextDigest) throw error;
      return helped;
    }
    return this.read(file);
  }

  recover(file, revision) {
    const claimFile = this.claimPath(file, revision);
    const claim = JSON.parse(readFileSync(claimFile, "utf8"));
    return this.recoverClaim(file, claim);
  }

  commit(file, expected, nextRecord) {
    const dir = path.dirname(file);
    mkdirSync(dir, { recursive: true, mode: 0o700 });
    const nextRevision = expected.revision + 1;
    const next = { ...nextRecord, revision: nextRevision };
    const token = randomUUID();
    const candidate = `${path.basename(file)}.cas.r${nextRevision}.${token}.candidate`;
    const candidatePath = path.join(dir, candidate);
    const claimCandidate = path.join(dir, `${path.basename(file)}.cas.r${nextRevision}.${token}.claim.tmp`);
    const claimFile = this.claimPath(file, nextRevision);
    const nextDigest = canonicalRecordDigest(next);
    const claim = {
      version: 1,
      expectedRevision: expected.revision,
      expectedDigest: expected.digest,
      nextRevision,
      nextDigest,
      candidate,
    };
    writeExclusive(candidatePath, Buffer.from(JSON.stringify(next) + "\n"));
    writeExclusive(claimCandidate, Buffer.from(JSON.stringify(claim) + "\n"));
    try {
      linkSync(claimCandidate, claimFile);
      fsyncDirectory(dir);
    } catch (error) {
      if (!error || error.code !== "EEXIST") throw error;
      try { this.recover(file, nextRevision); } finally {
        try { rmSync(candidatePath, { force: true }); } catch {}
        try { rmSync(claimCandidate, { force: true }); } catch {}
      }
      throw new DurableCASConflict("another process owns this durable revision");
    } finally {
      try { rmSync(claimCandidate, { force: true }); } catch {}
    }
    try {
      const committed = this.recoverClaim(file, claim);
      if (committed.revision !== nextRevision || committed.digest !== nextDigest) {
        throw new DurableCASConflict("durable revision was superseded before this writer committed");
      }
      return committed;
    } finally {
      try { rmSync(candidatePath, { force: true }); } catch {}
    }
  }

  cleanupArtifacts(file, { keepRevision = 0, orphanAgeMs = 60 * 60 * 1000 } = {}) {
    const dir = path.dirname(file);
    const prefix = `${path.basename(file)}.cas.`;
    try {
      for (const name of readdirSync(dir)) {
        if (!name.startsWith(prefix)) continue;
        const match = name.slice(prefix.length).match(/^r([0-9]+)\.(.*)$/);
        if (!match) continue;
        const revision = Number(match[1]);
        const suffix = match[2];
        if (!Number.isSafeInteger(revision)) continue;
        if (suffix === "claim") {
          if (revision < keepRevision) rmSync(path.join(dir, name), { force: true });
          continue;
        }
        if (!suffix.endsWith(".candidate") && !suffix.endsWith(".claim.tmp")) continue;
        let age = 0;
        try { age = Date.now() - statSync(path.join(dir, name)).mtimeMs; } catch { continue; }
        if (age >= orphanAgeMs && !existsSync(this.claimPath(file, revision))) {
          rmSync(path.join(dir, name), { force: true });
        }
      }
    } catch (error) {
      if (!error || error.code !== "ENOENT") throw error;
    }
  }

  sweepDirectory(dir, { orphanAgeMs = 60 * 60 * 1000 } = {}) {
    let removed = 0;
    try {
      for (const name of readdirSync(dir)) {
        const match = name.match(/^(.*)\.cas\.r([0-9]+)\.[^.]+\.(candidate|claim\.tmp)$/);
        if (!match) continue;
        const file = path.join(dir, match[1]);
        const revision = Number(match[2]);
        if (!Number.isSafeInteger(revision) || existsSync(this.claimPath(file, revision))) continue;
        const artifact = path.join(dir, name);
        let age = 0;
        try { age = Date.now() - statSync(artifact).mtimeMs; } catch { continue; }
        if (age < orphanAgeMs) continue;
        rmSync(artifact, { force: true });
        removed++;
      }
    } catch (error) {
      if (!error || error.code !== "ENOENT") throw error;
    }
    return removed;
  }
}
