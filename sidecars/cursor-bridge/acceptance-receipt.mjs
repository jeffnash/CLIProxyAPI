// Acceptance-phase receipt machine (TURN_RECEIPT_VERSION 6).
// Dual-read migrates v1–v5 status conservatively; new writes are v6-only
// with acceptancePhase. Mixed writable schemas are forbidden.

export const TURN_RECEIPT_VERSION = 6;

export const ACCEPTANCE_PHASE = Object.freeze({
  NOT_SENT: "NOT_SENT",
  PREPARED_DURABLE: "PREPARED_DURABLE",
  MAYBE_ACCEPTED: "MAYBE_ACCEPTED",
  ACCEPTED: "ACCEPTED",
  COMPLETED: "COMPLETED",
  REJECTED_BEFORE_SEND: "REJECTED_BEFORE_SEND",
});

/** Legacy v4/v5 fresh-attempt status values (dual-read only; never newly written). */
export const LEGACY_FRESH_ATTEMPT_STATE = Object.freeze({
  UNKNOWN: "unknown",
  RUNNING: "running",
  FAILED: "failed",
});

const PHASE = ACCEPTANCE_PHASE;

const LEGAL_TRANSITIONS = Object.freeze({
  [PHASE.NOT_SENT]: Object.freeze([PHASE.PREPARED_DURABLE, PHASE.REJECTED_BEFORE_SEND]),
  [PHASE.PREPARED_DURABLE]: Object.freeze([PHASE.MAYBE_ACCEPTED, PHASE.REJECTED_BEFORE_SEND]),
  [PHASE.MAYBE_ACCEPTED]: Object.freeze([PHASE.ACCEPTED, PHASE.COMPLETED]),
  [PHASE.ACCEPTED]: Object.freeze([PHASE.ACCEPTED, PHASE.COMPLETED]),
  [PHASE.COMPLETED]: Object.freeze([PHASE.COMPLETED]),
  [PHASE.REJECTED_BEFORE_SEND]: Object.freeze([]),
});

export class IllegalAcceptanceTransition extends Error {
  constructor(from, to) {
    super(`illegal acceptance-phase transition ${from} -> ${to}`);
    this.name = "IllegalAcceptanceTransition";
    this.code = "illegal_acceptance_transition";
    this.from = from;
    this.to = to;
  }
}

export class EnvelopeMutationError extends Error {
  constructor(message) {
    super(message || "acceptance receipt envelope is immutable after MAYBE_ACCEPTED");
    this.name = "EnvelopeMutationError";
    this.code = "acceptance_envelope_immutable";
  }
}

export function isAcceptancePhase(value) {
  return typeof value === "string" && Object.values(PHASE).includes(value);
}

export function isLegalAcceptanceTransition(from, to) {
  if (!isAcceptancePhase(from) || !isAcceptancePhase(to)) return false;
  return LEGAL_TRANSITIONS[from].includes(to);
}

export function assertLegalAcceptanceTransition(from, to) {
  if (isLegalAcceptanceTransition(from, to)) return;
  throw new IllegalAcceptanceTransition(from, to);
}

/**
 * Dual-read migration for v1–v5 (and raw status fields).
 * Conservative: legacy failure never becomes REJECTED_BEFORE_SEND.
 */
export function migrateLegacyAcceptancePhase(record) {
  if (!record || typeof record !== "object" || Array.isArray(record)) {
    return PHASE.NOT_SENT;
  }
  if (isAcceptancePhase(record.acceptancePhase)) {
    return record.acceptancePhase;
  }
  if (record.status === "completed") return PHASE.COMPLETED;
  if (record.status === "delivering") return PHASE.MAYBE_ACCEPTED;
  if (record.status === LEGACY_FRESH_ATTEMPT_STATE.UNKNOWN) return PHASE.MAYBE_ACCEPTED;
  if (record.status === LEGACY_FRESH_ATTEMPT_STATE.RUNNING) return PHASE.ACCEPTED;
  if (record.status === LEGACY_FRESH_ATTEMPT_STATE.FAILED) {
    if (hasPositiveAcceptanceEvidence(record)) return PHASE.ACCEPTED;
    return PHASE.MAYBE_ACCEPTED;
  }
  return PHASE.NOT_SENT;
}

export function resolveAcceptancePhase(record) {
  return migrateLegacyAcceptancePhase(record);
}

export function hasPositiveAcceptanceEvidence(record) {
  if (!record || typeof record !== "object") return false;
  if (record.acceptanceEvidence === "user_message_appended"
      || record.acceptanceEvidence === "agent_send_resolved") {
    return true;
  }
  if (Number.isFinite(record.runningAt) || Number.isFinite(record.acceptedAt)) return true;
  if (record.status === LEGACY_FRESH_ATTEMPT_STATE.RUNNING) return true;
  return false;
}

export function isUnresolvedAcceptancePhase(phase) {
  return phase === PHASE.PREPARED_DURABLE
    || phase === PHASE.MAYBE_ACCEPTED
    || phase === PHASE.ACCEPTED;
}

export function isUnresolvedAcceptanceRecord(record) {
  if (!record || typeof record !== "object") return false;
  const phase = resolveAcceptancePhase(record);
  if (isUnresolvedAcceptancePhase(phase)) return true;
  // Raw legacy statuses retained during dual-read even if migration is skipped.
  return record.status === "delivering"
    || record.status === LEGACY_FRESH_ATTEMPT_STATE.UNKNOWN
    || record.status === LEGACY_FRESH_ATTEMPT_STATE.RUNNING
    || record.status === LEGACY_FRESH_ATTEMPT_STATE.FAILED;
}

export function sendBoundaryCrossed(phase) {
  return phase === PHASE.MAYBE_ACCEPTED
    || phase === PHASE.ACCEPTED
    || phase === PHASE.COMPLETED;
}

export function allowsNewAttempt(phase) {
  return phase === PHASE.NOT_SENT || phase === PHASE.REJECTED_BEFORE_SEND;
}

export function recoveryActionForPhase(phase) {
  switch (phase) {
    case PHASE.PREPARED_DURABLE:
      return "transition_and_send_same_envelope";
    case PHASE.MAYBE_ACCEPTED:
      return "exact_reattachment_only";
    case PHASE.ACCEPTED:
      return "reattach_or_resume";
    case PHASE.COMPLETED:
      return "replay";
    case PHASE.REJECTED_BEFORE_SEND:
      return "new_attempt_ok";
    case PHASE.NOT_SENT:
    default:
      return "prepare_and_send";
  }
}

const ENVELOPE_KEYS = Object.freeze([
  "deliveryIdempotencyKey",
  "deliveryMessage",
  "deliveryAdvertise",
  "deliveryModel",
  "deliveryToolChoice",
  "deliverySeededSystem",
  "deliverySystemBlockIds",
  "deliveryHasImages",
  "agentId",
  "generation",
  "requestHash",
  "clientMessageId",
  "sessionId",
  "keyFingerprint",
  "identityPolicy",
  "requestKind",
]);

export function sameFrozenEnvelope(left, right) {
  if (!left || !right) return false;
  for (const key of ENVELOPE_KEYS) {
    if (!Object.prototype.hasOwnProperty.call(left, key)
        && !Object.prototype.hasOwnProperty.call(right, key)) {
      continue;
    }
    if (JSON.stringify(left[key]) !== JSON.stringify(right[key])) return false;
  }
  return true;
}

/**
 * Apply a phase transition onto a receipt record (pure).
 * Fail-closed on illegal/backward transitions and post-boundary envelope mutation.
 */
export function applyAcceptanceTransition(existing, nextPhase, details = {}) {
  const from = existing ? resolveAcceptancePhase(existing) : PHASE.NOT_SENT;
  assertLegalAcceptanceTransition(from, nextPhase);

  if (existing && sendBoundaryCrossed(from) && details.envelope) {
    if (!sameFrozenEnvelope(existing, details.envelope)) {
      throw new EnvelopeMutationError();
    }
  }

  const record = existing ? { ...existing } : {};
  record.version = TURN_RECEIPT_VERSION;
  record.acceptancePhase = nextPhase;
  // New writes must not carry legacy UNKNOWN/RUNNING/FAILED status.
  if (Object.prototype.hasOwnProperty.call(record, "status")
      && (record.status === LEGACY_FRESH_ATTEMPT_STATE.UNKNOWN
        || record.status === LEGACY_FRESH_ATTEMPT_STATE.RUNNING
        || record.status === LEGACY_FRESH_ATTEMPT_STATE.FAILED
        || record.status === "delivering")) {
    delete record.status;
  }

  delete record.failedAt;
  delete record.failure;
  delete record.runningAt;
  delete record.rejectedAt;
  delete record.rejectionReason;

  const now = Number.isFinite(details.nowMs) ? details.nowMs : Date.now();

  if (nextPhase === PHASE.PREPARED_DURABLE) {
    record.preparedAt = now;
  }
  if (nextPhase === PHASE.MAYBE_ACCEPTED) {
    record.maybeAcceptedAt = record.maybeAcceptedAt || now;
  }
  if (nextPhase === PHASE.ACCEPTED) {
    record.acceptedAt = record.acceptedAt || now;
    if (details.evidence) record.acceptanceEvidence = String(details.evidence);
  }
  if (nextPhase === PHASE.COMPLETED) {
    record.status = "completed";
    record.completedAt = record.completedAt || now;
  }
  if (nextPhase === PHASE.REJECTED_BEFORE_SEND) {
    if (sendBoundaryCrossed(from)) {
      throw new IllegalAcceptanceTransition(from, nextPhase);
    }
    record.rejectedAt = now;
    record.rejectionReason = String(details.reason || "rejected before send boundary")
      .replace(/\s+/g, " ")
      .slice(0, 4096);
  }

  return record;
}

export function buildPreparedDurableRecord(fields, nowMs = Date.now()) {
  return applyAcceptanceTransition(null, PHASE.PREPARED_DURABLE, { nowMs, ...fields });
}
