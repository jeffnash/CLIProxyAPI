import { createHash } from "node:crypto";
import { InMemoryStateStore, StateStoreClient, StateStoreError } from "./state-store-client.mjs";

export { InMemoryStateStore, StateStoreClient, StateStoreError };

export const STREAM_JOURNAL_VERSION = 1;
export const CAPABILITY_STREAM_RESUME_V1 = "stream-resume-v1";

const JOURNALED_TYPES = new Set(["text", "reasoning", "tool_call", "turn_end", "error", "terminal"]);

export function canonicalPayload(value) {
  return JSON.stringify(value === undefined ? null : value);
}

export function payloadDigest(value) {
  return createHash("sha256").update(canonicalPayload(value)).digest("hex");
}

export function eventId(invocationId, sequence) {
  return `${String(invocationId || "")}:${Number(sequence) || 0}`;
}

export function parseResumeCursor(lastEventId) {
  const raw = String(lastEventId || "").trim();
  if (!raw) return null;
  const idx = raw.lastIndexOf(":");
  if (idx <= 0) return null;
  const invocationId = raw.slice(0, idx);
  const sequence = Number(raw.slice(idx + 1));
  if (!invocationId || !Number.isSafeInteger(sequence) || sequence < 0) return null;
  return { invocationId, sequence };
}

export function formatSseFrame(payload, { invocationId = "", sequence = 0 } = {}) {
  const data = typeof payload === "string" ? payload : JSON.stringify(payload);
  const id = invocationId && sequence > 0 ? eventId(invocationId, sequence) : "";
  return id ? `id: ${id}\ndata: ${data}\n\n` : `data: ${data}\n\n`;
}

export function hasStreamResumeCapability(headerValue) {
  const raw = Array.isArray(headerValue) ? headerValue.join(",") : String(headerValue || "");
  return raw.split(",").some((part) => part.trim().toLowerCase() === CAPABILITY_STREAM_RESUME_V1);
}

/**
 * Durable stream journal: append+await commit before the caller may expose an event.
 */
export class StreamJournal {
  constructor(store, { defaultPhase = "accepted" } = {}) {
    if (!store || typeof store.appendJournal !== "function") {
      throw new StateStoreError("state_store_unavailable", "stream journal requires a state store");
    }
    this.store = store;
    this.defaultPhase = defaultPhase;
    this._chains = new Map(); // invocationId -> Promise
  }

  _enqueue(invocationId, task) {
    const key = String(invocationId || "");
    const prev = this._chains.get(key) || Promise.resolve();
    const next = prev.catch(() => {}).then(task);
    this._chains.set(key, next);
    return next;
  }

  /**
   * Commit one event and return only after the store acknowledges durability.
   * Callers MUST NOT write the event to the client until this resolves.
   */
  appendBeforeExpose(invocationId, type, payload, {
    acceptancePhase = this.defaultPhase,
    sequence = 0,
  } = {}) {
    const id = String(invocationId || "");
    const eventType = String(type || "");
    if (!id) {
      return Promise.reject(new StateStoreError("invalid_request", "invocation_id required before expose"));
    }
    if (!JOURNALED_TYPES.has(eventType)) {
      return Promise.reject(new StateStoreError("invalid_request", `unsupported journal type ${eventType}`));
    }
    return this._enqueue(id, async () => {
      const body = payload && typeof payload === "object" ? payload : { value: payload };
      const event = {
        version: STREAM_JOURNAL_VERSION,
        invocation_id: id,
        type: eventType,
        payload: body,
        payload_digest: payloadDigest(body),
        acceptance_phase: acceptancePhase,
      };
      if (Number.isSafeInteger(sequence) && sequence > 0) event.sequence = sequence;
      const resp = await this.store.appendJournal(event);
      const committed = (resp && resp.payload) || event;
      if (!committed.sequence) {
        throw new StateStoreError("state_store_bad_response", "journal append returned no sequence");
      }
      return {
        ...committed,
        event_id: eventId(id, committed.sequence),
        sse_frame: formatSseFrame(payload, { invocationId: id, sequence: committed.sequence }),
      };
    });
  }

  async readAfter(invocationId, fromSequence = 0, limit = 1024) {
    const resp = await this.store.readJournal(invocationId, fromSequence, limit);
    const events = (resp && resp.payload && resp.payload.events) || resp.payload || [];
    return Array.isArray(events) ? events : [];
  }
}

export function createStreamJournalFromEnv(env = process.env) {
  const socket = String(env.CLIPROXY_STATE_SOCKET || env.CURSOR_AGENT_STATE_SOCKET || "").trim();
  if (!socket) return null;
  return new StreamJournal(new StateStoreClient(socket));
}
