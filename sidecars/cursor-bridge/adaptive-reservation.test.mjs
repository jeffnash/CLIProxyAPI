import assert from "node:assert/strict";
import test from "node:test";
import {
  DEFAULT_ADAPTIVE_MEMORY_INITIAL_BYTES,
  ADMISSION_PRIORITY,
  nextGeometricBytes,
  AdaptiveMemoryBudget,
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
  assert.throws(() => other.ensure(1), /capacity is occupied/);
});

test("admission priority constants match plan ladder", () => {
  assert.ok(ADMISSION_PRIORITY.RECOVERY > ADMISSION_PRIORITY.TOOL_CONTINUATION);
  assert.ok(ADMISSION_PRIORITY.TOOL_CONTINUATION > ADMISSION_PRIORITY.STREAM_RESUME);
  assert.ok(ADMISSION_PRIORITY.STREAM_RESUME > ADMISSION_PRIORITY.FRESH);
});
