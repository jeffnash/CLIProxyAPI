// Adaptive in-memory reservation helpers for replay / journal tails.
// Starts small (~2MiB), grows geometrically before accepting more bytes,
// and shrinks after durable persistence.

export const DEFAULT_ADAPTIVE_MEMORY_INITIAL_BYTES = 2 * 1024 * 1024;

export const ADMISSION_PRIORITY = Object.freeze({
  FRESH: 10,
  STREAM_RESUME: 20,
  TOOL_CONTINUATION: 30,
  RECOVERY: 40,
});

export function nextGeometricBytes(current, needed, initial = DEFAULT_ADAPTIVE_MEMORY_INITIAL_BYTES, hardCap) {
  const need = Number(needed) || 0;
  if (need <= 0) return Math.max(0, Number(current) || 0);
  const cap = Number(hardCap);
  if (!Number.isFinite(cap) || cap <= 0) throw new Error("hard capacity cap required");
  if (need > cap) throw new Error("requested bytes exceed hard capacity cap");
  let start = Number(initial) || DEFAULT_ADAPTIVE_MEMORY_INITIAL_BYTES;
  if (start > cap) start = cap;
  let next = Math.max(Number(current) || 0, start);
  while (next < need) {
    const doubled = next * 2;
    if (!Number.isFinite(doubled) || doubled <= next || doubled > cap) {
      next = cap;
      break;
    }
    next = doubled;
  }
  if (next < need) throw new Error("geometric growth cannot cover requested bytes");
  return next;
}

/**
 * Process-local adaptive memory budget. `shared` may be injected so multiple
 * segments share one global used counter (tests / multi-session).
 */
export class AdaptiveMemoryBudget {
  constructor({
    limit,
    initial = DEFAULT_ADAPTIVE_MEMORY_INITIAL_BYTES,
    hardCap,
    shared = null,
  } = {}) {
    this.limit = Number(limit);
    this.initial = Number(initial) || DEFAULT_ADAPTIVE_MEMORY_INITIAL_BYTES;
    this.hardCap = Number(hardCap) || this.limit;
    this.shared = shared || { used: 0 };
    this.reserved = 0;
  }

  get used() {
    return this.shared.used;
  }

  ensure(neededBytes) {
    const need = Math.max(0, Number(neededBytes) || 0);
    if (need <= this.reserved) return { reserved: this.reserved, grew: false };
    const target = nextGeometricBytes(this.reserved, need, this.initial, this.hardCap);
    const delta = target - this.reserved;
    if (this.shared.used + delta > this.limit) {
      const err = new Error("process-wide durable replay memory capacity is occupied; retry this turn shortly");
      err.code = "local_replay_capacity";
      err.status = 503;
      throw err;
    }
    this.shared.used += delta;
    this.reserved = target;
    return { reserved: this.reserved, grew: delta > 0 };
  }

  shrinkTo(exactBytes) {
    const next = Math.max(0, Number(exactBytes) || 0);
    if (next >= this.reserved) return { reserved: this.reserved };
    const delta = this.reserved - next;
    this.shared.used = Math.max(0, this.shared.used - delta);
    this.reserved = next;
    return { reserved: this.reserved };
  }

  release() {
    if (this.reserved > 0) {
      this.shared.used = Math.max(0, this.shared.used - this.reserved);
      this.reserved = 0;
    }
  }
}
