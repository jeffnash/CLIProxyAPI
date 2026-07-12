import { createConnection } from "node:net";
import { randomUUID } from "node:crypto";

export const STATE_STORE_PROTOCOL_VERSION = 1;

export const StateStoreOp = Object.freeze({
  PING: "ping",
  APPEND_JOURNAL: "append_journal",
  READ_JOURNAL: "read_journal",
  TRANSITION_PHASE: "transition_phase",
  GET_INVOCATION: "get_invocation",
});

export class StateStoreError extends Error {
  constructor(code, message) {
    super(message || code || "state store error");
    this.name = "StateStoreError";
    this.code = code || "state_store_error";
  }
}

/**
 * Async newline-delimited JSON client for internal/durablestate.
 * Never performs local fsync — durability lives in the Go coordinator.
 */
export class StateStoreClient {
  constructor(socketPath, { timeoutMs = 15_000 } = {}) {
    this.socketPath = String(socketPath || "");
    this.timeoutMs = Number.isFinite(timeoutMs) && timeoutMs > 0 ? timeoutMs : 15_000;
  }

  async call(op, payload = undefined, { id = randomUUID() } = {}) {
    if (!this.socketPath) {
      throw new StateStoreError("state_store_unavailable", "durable state socket path is empty");
    }
    const request = {
      version: STATE_STORE_PROTOCOL_VERSION,
      id: String(id || randomUUID()),
      op: String(op || ""),
    };
    if (payload !== undefined) request.payload = payload;
    const line = `${JSON.stringify(request)}\n`;
    if (Buffer.byteLength(line) > (1 << 20)) {
      throw new StateStoreError("state_store_request_too_large", "state store request exceeds 1MiB");
    }

    return await new Promise((resolve, reject) => {
      let settled = false;
      let buffer = Buffer.alloc(0);
      const finish = (err, value) => {
        if (settled) return;
        settled = true;
        clearTimeout(timer);
        try { socket.destroy(); } catch {}
        if (err) reject(err);
        else resolve(value);
      };

      const socket = createConnection({ path: this.socketPath });
      const timer = setTimeout(() => {
        finish(new StateStoreError("state_store_timeout", `state store ${op} timed out after ${this.timeoutMs}ms`));
      }, this.timeoutMs);

      socket.on("connect", () => {
        socket.write(line, (err) => {
          if (err) finish(new StateStoreError("state_store_write_failed", err.message));
        });
      });
      socket.on("data", (chunk) => {
        buffer = Buffer.concat([buffer, chunk]);
        const nl = buffer.indexOf(0x0a);
        if (nl < 0) {
          if (buffer.length > (1 << 20)) {
            finish(new StateStoreError("state_store_response_too_large", "state store response exceeds 1MiB"));
          }
          return;
        }
        const raw = buffer.subarray(0, nl).toString("utf8");
        let resp;
        try {
          resp = JSON.parse(raw);
        } catch (error) {
          finish(new StateStoreError("state_store_bad_response", `invalid JSON response: ${error.message}`));
          return;
        }
        if (!resp || resp.ok !== true) {
          finish(new StateStoreError(
            (resp && resp.code) || "state_store_rejected",
            (resp && resp.error) || "state store rejected the request",
          ));
          return;
        }
        finish(null, resp);
      });
      socket.on("error", (error) => {
        finish(new StateStoreError("state_store_connect_failed", error.message));
      });
      socket.on("end", () => {
        if (!settled) {
          finish(new StateStoreError("state_store_closed", "state store closed before a complete response"));
        }
      });
    });
  }

  ping() {
    return this.call(StateStoreOp.PING);
  }

  appendJournal(event) {
    return this.call(StateStoreOp.APPEND_JOURNAL, { event });
  }

  readJournal(invocationId, fromSequence = 0, limit = 1024) {
    return this.call(StateStoreOp.READ_JOURNAL, {
      invocation_id: String(invocationId || ""),
      from_sequence: Number(fromSequence) || 0,
      limit: Number(limit) || 1024,
    });
  }
}

/**
 * Process-local durable-state stand-in for unit tests.
 * Assigns monotonic sequences and retains events without Unix I/O.
 */
export class InMemoryStateStore {
  constructor() {
    this.invocations = new Map(); // invocationId -> { phase, cursor }
    this.journals = new Map(); // invocationId -> events[]
  }

  ensureInvocation(invocationId, phase = "accepted") {
    const id = String(invocationId || "");
    if (!id) throw new StateStoreError("invalid_request", "invocation_id required");
    if (!this.invocations.has(id)) {
      this.invocations.set(id, { phase, cursor: 0 });
      this.journals.set(id, []);
    }
    return this.invocations.get(id);
  }

  async call(op, payload = undefined) {
    if (op === StateStoreOp.PING) {
      return { version: STATE_STORE_PROTOCOL_VERSION, ok: true, payload: { pong: true } };
    }
    if (op === StateStoreOp.APPEND_JOURNAL) {
      const event = payload && payload.event ? { ...payload.event } : null;
      if (!event || !event.invocation_id || !event.type) {
        throw new StateStoreError("invalid_request", "invocation_id and type required");
      }
      const inv = this.invocations.get(event.invocation_id);
      if (!inv) throw new StateStoreError("not_found", "invocation not found");
      const next = inv.cursor + 1;
      if (!event.sequence) event.sequence = next;
      if (event.sequence !== next) {
        throw new StateStoreError("conflict", `expected journal sequence ${next}, got ${event.sequence}`);
      }
      inv.cursor = next;
      event.created_at = event.created_at || new Date().toISOString();
      this.journals.get(event.invocation_id).push(event);
      return { version: STATE_STORE_PROTOCOL_VERSION, ok: true, payload: event };
    }
    if (op === StateStoreOp.READ_JOURNAL) {
      const id = String((payload && payload.invocation_id) || "");
      const from = Number((payload && payload.from_sequence) || 0);
      const limit = Math.max(1, Number((payload && payload.limit) || 1024));
      const all = this.journals.get(id) || [];
      const events = all.filter((ev) => ev.sequence > from).slice(0, limit);
      return { version: STATE_STORE_PROTOCOL_VERSION, ok: true, payload: { events } };
    }
    throw new StateStoreError("invalid_request", `unsupported op ${op}`);
  }

  appendJournal(event) {
    return this.call(StateStoreOp.APPEND_JOURNAL, { event });
  }

  readJournal(invocationId, fromSequence = 0, limit = 1024) {
    return this.call(StateStoreOp.READ_JOURNAL, {
      invocation_id: invocationId,
      from_sequence: fromSequence,
      limit,
    });
  }
}
