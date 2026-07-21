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

const DEFAULT_RETRY_AFTER_MS = 1_000;

function boundedInteger(value, name, { min = 0, allowInfinity = false } = {}) {
  const number = Number(value);
  if (allowInfinity && number === Number.POSITIVE_INFINITY) return number;
  if (!Number.isSafeInteger(number) || number < min) {
    throw new TypeError(`${name} must be a safe integer >= ${min}`);
  }
  return number;
}

export class CapacityAdmissionError extends Error {
  constructor(message, {
    code = "admission_capacity",
    status = 503,
    retryable = true,
    retryAfterMs = DEFAULT_RETRY_AFTER_MS,
    requestedBytes = 0,
    availableBytes = 0,
    priority = ADMISSION_PRIORITY.FRESH,
  } = {}) {
    super(message);
    this.name = "CapacityAdmissionError";
    this.code = code;
    this.status = status;
    this.retryable = retryable;
    this.retryAfterMs = retryable ? Math.max(0, Number(retryAfterMs) || 0) : 0;
    this.requestedBytes = Math.max(0, Number(requestedBytes) || 0);
    this.availableBytes = Math.max(0, Number(availableBytes) || 0);
    this.priority = Number(priority) || ADMISSION_PRIORITY.FRESH;
  }

  toJSON() {
    return {
      code: this.code,
      message: this.message,
      retryable: this.retryable,
      retryAfterMs: this.retryAfterMs,
      requestedBytes: this.requestedBytes,
      availableBytes: this.availableBytes,
      priority: this.priority,
    };
  }
}

function capacityError({ code = "durable_state_capacity", limit, used, requested, priority, retryAfterMs }) {
  return new CapacityAdmissionError(
    "shared durable capacity is occupied; retry after higher-priority or earlier work releases its reservation",
    {
      code,
      status: 507,
      retryable: true,
      retryAfterMs,
      requestedBytes: requested,
      availableBytes: Math.max(0, limit - used),
      priority,
    },
  );
}

/**
 * Immutable exact-byte ledger reducers. Persist the returned state with the
 * caller's CAS/transaction before treating a reservation as durable.
 */
export function exactReservationBytes(state) {
  const entries = state?.entries && typeof state.entries === "object" ? state.entries : {};
  return Object.values(entries).reduce((sum, entry) => {
    const bytes = Number(entry?.bytes);
    return sum + (Number.isSafeInteger(bytes) && bytes > 0 ? bytes : 0);
  }, 0);
}

export function exactUnmaterializedReservationBytes(state) {
  const entries = state?.entries && typeof state.entries === "object" ? state.entries : {};
  return Object.values(entries).reduce((sum, entry) => {
    const bytes = Number.isSafeInteger(entry?.bytes) && entry.bytes > 0 ? entry.bytes : 0;
    const materialized = Number.isSafeInteger(entry?.materializedBytes) && entry.materializedBytes > 0
      ? Math.min(bytes, entry.materializedBytes) : 0;
    return sum + Math.max(0, bytes - materialized);
  }, 0);
}

export function reserveExactBytes(state, key, bytes, {
  limit,
  createdAt = Date.now(),
  availableBytes = Number.POSITIVE_INFINITY,
  minFreeBytes = 0,
  priority = ADMISSION_PRIORITY.FRESH,
  retryAfterMs = DEFAULT_RETRY_AFTER_MS,
  maxEntries = 4_096,
} = {}) {
  const reservationKey = String(key || "");
  if (!reservationKey) throw new TypeError("reservation key is required");
  const target = boundedInteger(bytes, "reservation bytes");
  const hardLimit = boundedInteger(limit, "reservation limit", { min: 1 });
  const free = boundedInteger(availableBytes, "available bytes", { allowInfinity: true });
  const floor = boundedInteger(minFreeBytes, "minimum free bytes");
  const entries = state?.entries && typeof state.entries === "object" ? state.entries : {};
  const entryLimit = boundedInteger(maxEntries, "maximum reservation entries", { min: 1 });
  if (!Object.prototype.hasOwnProperty.call(entries, reservationKey)
      && Object.keys(entries).length >= entryLimit) {
    throw new CapacityAdmissionError("durable reservation ledger entry capacity is occupied", {
      code: "durable_state_capacity",
      status: 507,
      retryable: true,
      retryAfterMs,
      requestedBytes: target,
      availableBytes: 0,
      priority,
    });
  }
  const existing = Number.isSafeInteger(entries[reservationKey]?.bytes)
    ? Math.max(0, entries[reservationKey].bytes) : 0;
  if (existing >= target) return { state, delta: 0, reservedBytes: exactReservationBytes(state) };
  const used = exactReservationBytes(state);
  const unmaterialized = exactUnmaterializedReservationBytes(state);
  const delta = target - existing;
  // statfs free space already excludes receipt bytes which are on disk. Only
  // reservations not yet materialized are outstanding claims against it.
  if (used + delta > hardLimit || free - unmaterialized - delta < floor) {
    const available = Math.max(0, Math.min(hardLimit - used, free - floor - unmaterialized));
    throw capacityError({ limit: used + available, used, requested: delta, priority, retryAfterMs });
  }
  const next = {
    version: 1,
    entries: {
      ...entries,
      [reservationKey]: {
        bytes: target,
        createdAt: Number.isSafeInteger(entries[reservationKey]?.createdAt)
          ? entries[reservationKey].createdAt : boundedInteger(createdAt, "reservation creation time"),
        materializedBytes: Number.isSafeInteger(entries[reservationKey]?.materializedBytes)
          ? Math.min(target, Math.max(0, entries[reservationKey].materializedBytes)) : 0,
      },
    },
  };
  return { state: next, delta, reservedBytes: used + delta };
}

export function resizeExactBytes(state, key, bytes, { materialized = false } = {}) {
  const reservationKey = String(key || "");
  const target = boundedInteger(bytes, "reservation bytes");
  const entries = state?.entries && typeof state.entries === "object" ? state.entries : {};
  if (!entries[reservationKey]) return state;
  const nextMaterialized = materialized
    ? target
    : Math.min(target, Math.max(0, Number(entries[reservationKey].materializedBytes) || 0));
  if (entries[reservationKey].bytes === target
      && (Number(entries[reservationKey].materializedBytes) || 0) === nextMaterialized) return state;
  if (target > entries[reservationKey].bytes) {
    throw new RangeError("reservation growth must use reserveExactBytes so capacity is checked");
  }
  return {
    version: 1,
    entries: {
      ...entries,
      [reservationKey]: { ...entries[reservationKey], bytes: target, materializedBytes: nextMaterialized },
    },
  };
}

export function releaseExactBytes(state, key) {
  const reservationKey = String(key || "");
  const entries = state?.entries && typeof state.entries === "object" ? state.entries : {};
  if (!entries[reservationKey]) return state;
  const next = { ...entries };
  delete next[reservationKey];
  return { version: 1, entries: next };
}

export function reconcileExactBytes(state, observed, {
  orphanCutoff = Number.NEGATIVE_INFINITY,
  preserveKeys = [],
} = {}) {
  const current = state?.entries && typeof state.entries === "object" ? state.entries : {};
  const authoritative = observed && typeof observed === "object" ? observed : {};
  const preserved = new Set(Array.isArray(preserveKeys) ? preserveKeys.map(String) : []);
  const entries = { ...authoritative };
  for (const [key, entry] of Object.entries(current)) {
    if (!entries[key] && (preserved.has(key) || Number(entry?.createdAt) >= orphanCutoff)) entries[key] = entry;
  }
  return { version: 1, entries };
}

class AdmissionLease {
  constructor(queue, id, bytes, priority) {
    this.queue = queue;
    this.id = id;
    this.bytes = bytes;
    this.priority = priority;
    this.released = false;
  }

  release() {
    if (this.released) return false;
    this.released = true;
    this.queue._release(this);
    return true;
  }
}

/**
 * Process-local priority admission queue. It performs exact accounting,
 * priority/FIFO ordering, work-conserving fit, and bounded anti-starvation.
 * Callers remain responsible for persisting durable reservations before
 * external side effects.
 */
export class PriorityAdmissionQueue {
  constructor({
    limit,
    maxQueued = 1_024,
    retryAfterMs = DEFAULT_RETRY_AFTER_MS,
    maxBypasses = 32,
  } = {}) {
    this.limit = boundedInteger(limit, "admission limit", { min: 1 });
    this.maxQueued = boundedInteger(maxQueued, "maximum queued admissions", { min: 1 });
    this.retryAfterMs = boundedInteger(retryAfterMs, "retry-after milliseconds");
    this.maxBypasses = boundedInteger(maxBypasses, "maximum admission bypasses", { min: 1 });
    this.used = 0;
    this.sequence = 0;
    this.waiters = [];
    this.leases = new Map();
  }

  get queued() { return this.waiters.length; }
  get available() { return this.limit - this.used; }

  tryAcquire({ bytes, priority = ADMISSION_PRIORITY.FRESH, id = "" } = {}) {
    const requested = this._validateRequest(bytes, priority);
    if (this.waiters.length > 0 || requested > this.available) {
      throw capacityError({
        code: "admission_capacity",
        limit: this.limit,
        used: this.used,
        requested,
        priority,
        retryAfterMs: this.retryAfterMs,
      });
    }
    return this._grant({ bytes: requested, priority, id: String(id || `admission-${++this.sequence}`) });
  }

  acquire({ bytes, priority = ADMISSION_PRIORITY.FRESH, id = "", signal } = {}) {
    const requested = this._validateRequest(bytes, priority);
    if (signal?.aborted) return Promise.reject(signal.reason || new Error("admission aborted"));
    if (this.waiters.length === 0 && requested <= this.available) {
      return Promise.resolve(this._grant({
        bytes: requested,
        priority,
        id: String(id || `admission-${++this.sequence}`),
      }));
    }
    if (this.waiters.length >= this.maxQueued) {
      return Promise.reject(new CapacityAdmissionError("admission queue is full; retry shortly", {
        code: "admission_queue_capacity",
        status: 503,
        retryable: true,
        retryAfterMs: this.retryAfterMs,
        requestedBytes: requested,
        availableBytes: this.available,
        priority,
      }));
    }
    return new Promise((resolve, reject) => {
      const waiter = {
        bytes: requested,
        priority,
        id: String(id || `admission-${++this.sequence}`),
        sequence: ++this.sequence,
        resolve,
        reject,
        signal,
        bypasses: 0,
      };
      if (signal) {
        waiter.onAbort = () => {
          const index = this.waiters.indexOf(waiter);
          if (index >= 0) this.waiters.splice(index, 1);
          reject(signal.reason || new Error("admission aborted"));
          this._drain();
        };
        signal.addEventListener("abort", waiter.onAbort, { once: true });
      }
      this.waiters.push(waiter);
      this.waiters.sort((a, b) => b.priority - a.priority || a.sequence - b.sequence);
      this._drain();
    });
  }

  _validateRequest(bytes, priority) {
    const requested = boundedInteger(bytes, "admission bytes", { min: 1 });
    boundedInteger(priority, "admission priority", { min: 1 });
    if (requested > this.limit) {
      throw new CapacityAdmissionError("one admission exceeds the configured capacity limit", {
        code: "admission_request_too_large",
        status: 507,
        retryable: false,
        requestedBytes: requested,
        availableBytes: this.limit,
        priority,
      });
    }
    return requested;
  }

  _grant({ bytes, priority, id }) {
    let uniqueId = id;
    while (this.leases.has(uniqueId)) uniqueId = `${id}-${++this.sequence}`;
    const lease = new AdmissionLease(this, uniqueId, bytes, priority);
    this.used += bytes;
    this.leases.set(uniqueId, lease);
    return lease;
  }

  _release(lease) {
    if (this.leases.get(lease.id) !== lease) return;
    this.leases.delete(lease.id);
    this.used = Math.max(0, this.used - lease.bytes);
    this._drain();
  }

  _drain() {
    while (this.waiters.length > 0) {
      const protectedWaiter = this.waiters
        .filter((waiter) => waiter.bypasses >= this.maxBypasses)
        .sort((a, b) => a.sequence - b.sequence)[0];
      let index;
      if (protectedWaiter) {
        if (protectedWaiter.bytes > this.available) return;
        index = this.waiters.indexOf(protectedWaiter);
      } else {
        // Be work-conserving while preserving the priority ordering whenever
        // the preferred request fits. A bounded bypass count eventually fences
        // every older waiter, so neither a large recovery nor a fresh turn can
        // starve under a stream of smaller/higher-priority arrivals.
        index = this.waiters.findIndex((waiter) => waiter.bytes <= this.available);
        if (index < 0) return;
      }
      const [waiter] = this.waiters.splice(index, 1);
      for (const skipped of this.waiters) {
        if (skipped.sequence < waiter.sequence) skipped.bypasses++;
      }
      if (waiter.signal && waiter.onAbort) {
        waiter.signal.removeEventListener("abort", waiter.onAbort);
      }
      waiter.resolve(this._grant(waiter));
    }
  }
}

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
      throw new CapacityAdmissionError(
        "process-wide durable replay memory capacity is occupied; retry this turn shortly",
        {
          code: "local_replay_capacity",
          status: 503,
          retryable: true,
          requestedBytes: delta,
          availableBytes: Math.max(0, this.limit - this.shared.used),
        },
      );
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
