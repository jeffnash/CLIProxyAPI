import assert from "node:assert/strict";
import { existsSync, mkdtempSync, readFileSync, readdirSync, utimesSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import test from "node:test";

import {
  canonicalRecordDigest,
  DurableCASConflict,
  DurableJSONCAS,
} from "./durable-json-cas.mjs";

function tempFile(t) {
  const dir = mkdtempSync(path.join(tmpdir(), "durable-json-cas-"));
  t.after(() => import("node:fs").then(({ rmSync }) => rmSync(dir, { recursive: true, force: true })));
  return path.join(dir, "record.json");
}

test("immutable revision claim fences a stale writer", (t) => {
  const file = tempFile(t);
  const first = new DurableJSONCAS();
  const second = new DurableJSONCAS();
  const revision1 = first.commit(file, first.read(file), { value: "initial" });
  const stale = { ...revision1 };
  const winner = second.commit(file, second.read(file), { value: "winner" });
  assert.equal(winner.record.revision, 2);
  assert.throws(
    () => first.commit(file, stale, { value: "stale" }),
    (error) => error instanceof DurableCASConflict,
  );
  assert.equal(first.readRecover(file).record.value, "winner");
});

test("a reader completes a claimed candidate after owner death", (t) => {
  const file = tempFile(t);
  const cas = new DurableJSONCAS();
  const expected = cas.read(file);
  const next = { value: "recovered", revision: 1 };
  const candidate = `${path.basename(file)}.cas.r1.crashed.candidate`;
  writeFileSync(path.join(path.dirname(file), candidate), JSON.stringify(next) + "\n");
  writeFileSync(cas.claimPath(file, 1), JSON.stringify({
    version: 1,
    expectedRevision: expected.revision,
    expectedDigest: expected.digest,
    nextRevision: 1,
    nextDigest: canonicalRecordDigest(next),
    candidate,
  }) + "\n");

  assert.deepEqual(cas.readRecover(file).record, next);
  assert.deepEqual(JSON.parse(readFileSync(file, "utf8")), next);
});

test("claim digest mismatch fails closed", (t) => {
  const file = tempFile(t);
  const cas = new DurableJSONCAS();
  const candidate = `${path.basename(file)}.cas.r1.corrupt.candidate`;
  writeFileSync(path.join(path.dirname(file), candidate), JSON.stringify({ value: "actual", revision: 1 }) + "\n");
  writeFileSync(cas.claimPath(file, 1), JSON.stringify({
    version: 1,
    expectedRevision: 0,
    expectedDigest: "",
    nextRevision: 1,
    nextDigest: "0".repeat(64),
    candidate,
  }) + "\n");
  assert.throws(() => cas.readRecover(file), /digest is invalid/);
});

test("canonical progress compacts old claims and sweeps unclaimed aged candidates", (t) => {
  const file = tempFile(t);
  const cas = new DurableJSONCAS();
  let state = cas.read(file);
  for (let revision = 1; revision <= 8; revision++) {
    state = cas.commit(file, state, { value: revision });
    cas.cleanupArtifacts(file, { keepRevision: state.revision, orphanAgeMs: 0 });
  }
  const prefix = `${path.basename(file)}.cas.`;
  const claims = readdirSync(path.dirname(file)).filter((name) => name.startsWith(prefix) && name.endsWith(".claim"));
  assert.deepEqual(claims, [`${path.basename(file)}.cas.r8.claim`]);

  const orphan = `${file}.cas.r9.orphan.candidate`;
  writeFileSync(orphan, "orphan\n");
  const old = new Date(0);
  utimesSync(orphan, old, old);
  cas.sweepDirectory(path.dirname(file), { orphanAgeMs: 1 });
  assert.equal(existsSync(orphan), false);
});
