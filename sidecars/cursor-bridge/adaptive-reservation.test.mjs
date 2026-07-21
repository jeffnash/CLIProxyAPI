import assert from "node:assert/strict";
import test from "node:test";
import {
  DEFAULT_ADAPTIVE_MEMORY_INITIAL_BYTES,
  ADMISSION_PRIORITY,
  CapacityAdmissionError,
  nextGeometricBytes,
  AdaptiveMemoryBudget,
  PriorityAdmissionQueue,
  exactReservationBytes,
  exactUnmaterializedReservationBytes,
  reserveExactBytes,
  resizeExactBytes,
  releaseExactBytes,
  reconcileExactBytes,
} from "./adaptive-reservation.mjs";

test("nextGeometricBytes grows from 2MiB toward need without exceeding cap", () => {
  const initial = DEFAULT_ADAPTIVE_MEMORY_INITIAL_BYTES;
  assert.equal(nextGeometricBytes(0, 100, initial, 64 << 20), initial);
  assert.equal(nextGeometricBytes(initial, initial + 1, initial, 64 << 20), initial * 2);
  assert.equal(nextGeometricBytes(initial, (64 << 20) - 1, initial, 64 << 20), 64 << 20);
  assert.throws(() => nextGeometricBytes(0, (65 << 20), initial, 64 << 20), /exceed hard capacity/);
});

test("AdaptiveMemoryBudget reserves initial then grows and shrinks", () => {
  const budget = new AdaptiveMemoryBudget({ limit: 32 << 20, initial: 2 << 20, hardCap: 16 << 20 });
  const first = budget.ensure(100);
  assert.equal(first.reserved, 2 << 20);
  assert.equal(budget.used, 2 << 20);
  const grown = budget.ensure((2 << 20) + 10);
  assert.equal(grown.reserved, 4 << 20);
  assert.equal(budget.used, 4 << 20);
  const shrunk = budget.shrinkTo(1000);
  assert.equal(shrunk.reserved, 1000);
  assert.equal(budget.used, 1000);
  budget.release();
  assert.equal(budget.used, 0);
  assert.equal(budget.reserved, 0);
});

test("AdaptiveMemoryBudget fails closed when global limit is exhausted", () => {
  const budget = new AdaptiveMemoryBudget({ limit: 3 << 20, initial: 2 << 20, hardCap: 8 << 20 });
  budget.ensure(1);
  const other = new AdaptiveMemoryBudget({
    limit: budget.limit,
    initial: 2 << 20,
    hardCap: 8 << 20,
    shared: budget.shared,
  });
  assert.throws(() => other.ensure(1), (error) => {
    assert.ok(error instanceof CapacityAdmissionError);
    assert.equal(error.code, "local_replay_capacity");
    assert.equal(error.retryable, true);
    assert.ok(error.retryAfterMs > 0);
    return true;
  });
});

test("admission priority constants match plan ladder", () => {
  assert.ok(ADMISSION_PRIORITY.RECOVERY > ADMISSION_PRIORITY.TOOL_CONTINUATION);
  assert.ok(ADMISSION_PRIORITY.TOOL_CONTINUATION > ADMISSION_PRIORITY.STREAM_RESUME);
  assert.ok(ADMISSION_PRIORITY.STREAM_RESUME > ADMISSION_PRIORITY.FRESH);
});

test("exact reservation reducers grow, shrink, release, and reconcile crash orphans", () => {
  let state = { version: 1, entries: {} };
  let result = reserveExactBytes(state, "turn-a", 128, {
    limit: 1_024,
    availableBytes: 2_048,
    minFreeBytes: 256,
    createdAt: 100,
  });
  state = result.state;
  assert.equal(result.delta, 128);
  assert.equal(exactReservationBytes(state), 128);

  result = reserveExactBytes(state, "turn-a", 256, {
    limit: 1_024,
    availableBytes: 2_048,
    minFreeBytes: 256,
    createdAt: 999,
  });
  state = result.state;
  assert.equal(result.delta, 128);
  assert.equal(state.entries["turn-a"].createdAt, 100, "growth retains the crash-orphan clock");

  state = resizeExactBytes(state, "turn-a", 64);
  assert.equal(exactReservationBytes(state), 64);
  assert.throws(() => resizeExactBytes(state, "turn-a", 65), /growth must use reserveExactBytes/);
  state = reserveExactBytes(state, "turn-b", 100, {
    limit: 1_024,
    availableBytes: 2_048,
    createdAt: 300,
  }).state;
  state = reconcileExactBytes(state, { "turn-b": { bytes: 80, createdAt: 300 } }, { orphanCutoff: 200 });
  assert.deepEqual(state.entries, { "turn-b": { bytes: 80, createdAt: 300 } });
  state = releaseExactBytes(state, "turn-b");
  assert.equal(exactReservationBytes(state), 0);
});

test("reconciliation never TTL-frees explicitly preserved corrupt evidence", () => {
  const state = {
    version: 1,
    entries: {
      corrupt: { bytes: 77, materializedBytes: 77, createdAt: 1 },
      orphan: { bytes: 11, materializedBytes: 0, createdAt: 1 },
    },
  };
  const reconciled = reconcileExactBytes(state, {}, {
    orphanCutoff: 100,
    preserveKeys: ["corrupt"],
  });
  assert.deepEqual(reconciled.entries, {
    corrupt: { bytes: 77, materializedBytes: 77, createdAt: 1 },
  });
});

test("exact reservation reducers fail with typed retry metadata before overcommit", () => {
  const state = reserveExactBytes({ version: 1, entries: {} }, "held", 900, {
    limit: 1_024,
    availableBytes: 4_096,
  }).state;
  assert.throws(() => reserveExactBytes(state, "next", 200, {
    limit: 1_024,
    availableBytes: 4_096,
    priority: ADMISSION_PRIORITY.FRESH,
    retryAfterMs: 2_500,
  }), (error) => {
    assert.equal(error.code, "durable_state_capacity");
    assert.equal(error.status, 507);
    assert.equal(error.retryable, true);
    assert.equal(error.retryAfterMs, 2_500);
    assert.equal(error.requestedBytes, 200);
    assert.equal(error.availableBytes, 124);
    return true;
  });
});

test("disk-floor accounting does not count materialized receipt bytes twice", () => {
  let state = reserveExactBytes({ version: 1, entries: {} }, "on-disk", 900, {
    limit: 2_000,
    availableBytes: 1_000,
  }).state;
  state = resizeExactBytes(state, "on-disk", 900, { materialized: true });
  assert.equal(exactReservationBytes(state), 900);
  assert.equal(exactUnmaterializedReservationBytes(state), 0);
  assert.doesNotThrow(() => reserveExactBytes(state, "new", 100, {
    limit: 2_000,
    availableBytes: 200,
    minFreeBytes: 100,
  }));
});

test("exact reservation ledger work is bounded by an explicit entry ceiling", () => {
  const full = { version: 1, entries: { a: { bytes: 1, createdAt: 1 }, b: { bytes: 1, createdAt: 1 } } };
  assert.throws(
    () => reserveExactBytes(full, "c", 1, { limit: 100, maxEntries: 2 }),
    (error) => error instanceof CapacityAdmissionError
      && error.code === "durable_state_capacity" && error.retryable === true,
  );
  assert.doesNotThrow(() => reserveExactBytes(full, "a", 1, { limit: 100, maxEntries: 2 }));
});

test("priority admission grants recovery before continuation, resume, and fresh", async () => {
  const queue = new PriorityAdmissionQueue({ limit: 10 });
  const blocker = queue.tryAcquire({ bytes: 10, id: "blocker" });
  const order = [];
  const fresh = queue.acquire({ bytes: 10, priority: ADMISSION_PRIORITY.FRESH, id: "fresh" })
    .then((lease) => { order.push("fresh"); return lease; });
  const resume = queue.acquire({ bytes: 10, priority: ADMISSION_PRIORITY.STREAM_RESUME, id: "resume" })
    .then((lease) => { order.push("resume"); return lease; });
  const continuation = queue.acquire({ bytes: 10, priority: ADMISSION_PRIORITY.TOOL_CONTINUATION, id: "continuation" })
    .then((lease) => { order.push("continuation"); return lease; });
  const recovery = queue.acquire({ bytes: 10, priority: ADMISSION_PRIORITY.RECOVERY, id: "recovery" })
    .then((lease) => { order.push("recovery"); return lease; });

  blocker.release();
  const recoveryLease = await recovery;
  recoveryLease.release();
  const continuationLease = await continuation;
  continuationLease.release();
  const resumeLease = await resume;
  resumeLease.release();
  const freshLease = await fresh;
  freshLease.release();
  assert.deepEqual(order, ["recovery", "continuation", "resume", "fresh"]);
  assert.equal(queue.used, 0);
});

test("priority admission is FIFO within a class and never overcommits under concurrency", async () => {
  const queue = new PriorityAdmissionQueue({ limit: 4 });
  const blocker = queue.tryAcquire({ bytes: 4 });
  const order = [];
  const waits = [1, 2, 3].map((id) => queue.acquire({
    bytes: 2,
    priority: ADMISSION_PRIORITY.TOOL_CONTINUATION,
    id: `continuation-${id}`,
  }).then((lease) => { order.push(id); return lease; }));
  blocker.release();
  const first = await waits[0];
  const second = await waits[1];
  assert.equal(queue.used, 4);
  assert.deepEqual(order, [1, 2]);
  first.release();
  const third = await waits[2];
  assert.deepEqual(order, [1, 2, 3]);
  second.release();
  third.release();
  assert.equal(queue.used, 0);
});

test("priority admission supports abort, bounded queues, and non-retryable oversize", async () => {
  const queue = new PriorityAdmissionQueue({ limit: 2, maxQueued: 1, retryAfterMs: 3_000 });
  const blocker = queue.tryAcquire({ bytes: 2 });
  const controller = new AbortController();
  const waiting = queue.acquire({ bytes: 1, signal: controller.signal });
  await assert.rejects(queue.acquire({ bytes: 1 }), (error) => {
    assert.equal(error.code, "admission_queue_capacity");
    assert.equal(error.retryAfterMs, 3_000);
    return true;
  });
  controller.abort(new Error("caller disconnected"));
  await assert.rejects(waiting, /caller disconnected/);
  assert.equal(queue.queued, 0);
  assert.throws(() => queue.tryAcquire({ bytes: 3 }), (error) => {
    assert.equal(error.code, "admission_request_too_large");
    assert.equal(error.retryable, false);
    return true;
  });
  blocker.release();
});

test("admission lease ids remain unique across generated suffix collisions", () => {
  const queue = new PriorityAdmissionQueue({ limit: 3 });
  const first = queue.tryAcquire({ bytes: 1, id: "same" });
  const occupiedSuffix = queue.tryAcquire({ bytes: 1, id: "same-1" });
  queue.sequence = 0;
  const duplicate = queue.tryAcquire({ bytes: 1, id: "same" });
  assert.notEqual(duplicate.id, occupiedSuffix.id);
  assert.equal(queue.leases.size, 3);
  first.release();
  occupiedSuffix.release();
  duplicate.release();
  assert.equal(queue.used, 0);
});

test("admission is work-conserving but bounds bypass of a large recovery", async () => {
  const queue = new PriorityAdmissionQueue({ limit: 10, maxBypasses: 2 });
  const blocker = queue.tryAcquire({ bytes: 9, id: "blocker" });
  const recoveryPromise = queue.acquire({ bytes: 10, priority: ADMISSION_PRIORITY.RECOVERY, id: "recovery" });
  const firstPromise = queue.acquire({ bytes: 1, priority: ADMISSION_PRIORITY.TOOL_CONTINUATION, id: "first" });
  const first = await firstPromise;
  const secondPromise = queue.acquire({ bytes: 1, priority: ADMISSION_PRIORITY.TOOL_CONTINUATION, id: "second" });
  first.release();
  const second = await secondPromise;
  let thirdGranted = false;
  const thirdPromise = queue.acquire({ bytes: 1, priority: ADMISSION_PRIORITY.TOOL_CONTINUATION, id: "third" })
    .then((lease) => { thirdGranted = true; return lease; });
  second.release();
  await Promise.resolve();
  assert.equal(thirdGranted, false, "the bypass bound must fence the waiting recovery");
  blocker.release();
  const recovery = await recoveryPromise;
  recovery.release();
  const third = await thirdPromise;
  third.release();
  assert.equal(queue.used, 0);
});

test("continuous later recovery arrivals cannot starve an older fresh turn", async () => {
  const queue = new PriorityAdmissionQueue({ limit: 1, maxBypasses: 2 });
  const blocker = queue.tryAcquire({ bytes: 1, id: "blocker" });
  let freshGranted = false;
  const freshPromise = queue.acquire({ bytes: 1, priority: ADMISSION_PRIORITY.FRESH, id: "fresh" })
    .then((lease) => { freshGranted = true; return lease; });
  const recoveryOnePromise = queue.acquire({ bytes: 1, priority: ADMISSION_PRIORITY.RECOVERY, id: "recovery-1" });
  blocker.release();
  const recoveryOne = await recoveryOnePromise;
  const recoveryTwoPromise = queue.acquire({ bytes: 1, priority: ADMISSION_PRIORITY.RECOVERY, id: "recovery-2" });
  recoveryOne.release();
  const recoveryTwo = await recoveryTwoPromise;
  const recoveryThreePromise = queue.acquire({ bytes: 1, priority: ADMISSION_PRIORITY.RECOVERY, id: "recovery-3" });
  recoveryTwo.release();
  const fresh = await freshPromise;
  assert.equal(freshGranted, true);
  fresh.release();
  const recoveryThree = await recoveryThreePromise;
  recoveryThree.release();
  assert.equal(queue.used, 0);
});

test("one hundred thirty small fresh starts queue without a fixed reservation cliff", async () => {
  const twoMiB = 2 * 1024 * 1024;
  const queue = new PriorityAdmissionQueue({ limit: 128 * 1024 * 1024, maxQueued: 128 });
  const active = [];
  for (let index = 0; index < 64; index++) {
    active.push(await queue.acquire({ bytes: twoMiB, priority: ADMISSION_PRIORITY.FRESH, id: `fresh-${index}` }));
  }
  const queued = Array.from({ length: 66 }, (_, offset) => queue.acquire({
    bytes: twoMiB,
    priority: ADMISSION_PRIORITY.FRESH,
    id: `fresh-${64 + offset}`,
  }));
  await Promise.resolve();
  assert.equal(queue.queued, 66);
  for (const lease of active) lease.release();
  const nextWave = await Promise.all(queued.slice(0, 64));
  assert.equal(queue.queued, 2);
  for (const lease of nextWave) lease.release();
  const finalWave = await Promise.all(queued.slice(64));
  for (const lease of finalWave) lease.release();
  assert.equal(queue.used, 0);
});

test("P9: 65 simultaneous starts queue without a cliff and a recovery preempts 54 queued fresh turns", async () => {
  const queue = new PriorityAdmissionQueue({ limit: 20 * 1024 * 1024, maxQueued: 1024 });
  const MiB2 = 2 * 1024 * 1024;
  // 10 fresh starts fit immediately (10 × 2MiB = 20MiB limit).
  const active = [];
  for (let i = 0; i < 10; i++) {
    active.push(await queue.acquire({ bytes: MiB2, priority: ADMISSION_PRIORITY.FRESH }));
  }
  // 54 more fresh starts queue (65 simultaneous subagent starts total with the recovery below).
  const pendingFresh = [];
  for (let i = 0; i < 54; i++) {
    pendingFresh.push(queue.acquire({ bytes: MiB2, priority: ADMISSION_PRIORITY.FRESH }));
  }
  assert.equal(queue.queued, 54);
  // A recovery arrives behind 54 queued fresh turns (55 waiters total).
  let recoveryGranted = false;
  const recoveryLease = queue.acquire({ bytes: MiB2, priority: ADMISSION_PRIORITY.RECOVERY })
    .then((lease) => { recoveryGranted = true; return lease; });
  assert.equal(queue.queued, 55);
  // Free one active slot: the recovery must win the grant ahead of all 54 fresh waiters.
  active[0].release();
  const recovery = await recoveryLease;
  assert.equal(recoveryGranted, true, "recovery preempts the queued fresh backlog");
  assert.equal(queue.queued, 54, "exactly the recovery was admitted");
  // No 507 cliff anywhere: drain the rest by releasing as grants arrive.
  const drainer = (async () => {
    for (const promise of pendingFresh) {
      const lease = await promise;
      lease.release();
    }
  })();
  active.slice(1).forEach((lease) => lease.release());
  recovery.release();
  await drainer;
  assert.equal(queue.queued, 0);
  assert.equal(queue.available, 20 * 1024 * 1024, "full capacity released, accounting exact");
});
