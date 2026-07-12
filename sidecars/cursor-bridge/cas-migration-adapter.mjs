/**
 * Dual-read migration adapter: legacy DurableJSONCAS file receipts remain
 * readable while Go durable-state becomes the long-term write path.
 */
import { readdirSync, readFileSync, statSync } from "node:fs";
import path from "node:path";
import { migrateLegacyAcceptancePhase, ACCEPTANCE_PHASE } from "./acceptance-receipt.mjs";

export function mapLegacyReceiptPhase(record) {
  if (!record || typeof record !== "object") return ACCEPTANCE_PHASE.PREPARED_DURABLE;
  if (record.acceptancePhase) return String(record.acceptancePhase).toLowerCase();
  return String(migrateLegacyAcceptancePhase(record) || ACCEPTANCE_PHASE.MAYBE_ACCEPTED).toLowerCase();
}

export function receiptToImportPayload(record, filePath = "") {
  const phase = mapLegacyReceiptPhase(record);
  return {
    path: filePath,
    invocation_id: String(record.invocationId || record.invocation_id || record.clientMessageId || ""),
    phase,
    envelope_digest: String(record.envelopeDigest || record.envelope_digest || record.deliveryIdempotencyKey || ""),
    request_hash: String(record.requestHash || record.request_hash || ""),
    tenant_id: String(record.tenantId || record.tenant_id || ""),
    conversation_id: String(record.sessionId || record.conversation_id || ""),
    auth_id: String(record.agentId || record.auth_id || ""),
    idempotency_key: String(record.deliveryIdempotencyKey || record.idempotencyKey || ""),
    raw_version: Number.isFinite(record.version) ? Number(record.version) : 0,
  };
}

export function scanLegacyReceiptDir(root) {
  const out = [];
  const stack = [root];
  while (stack.length) {
    const dir = stack.pop();
    let entries = [];
    try {
      entries = readdirSync(dir);
    } catch {
      continue;
    }
    for (const name of entries) {
      const full = path.join(dir, name);
      let st;
      try {
        st = statSync(full);
      } catch {
        continue;
      }
      if (st.isDirectory()) {
        stack.push(full);
        continue;
      }
      if (!name.toLowerCase().endsWith(".json")) continue;
      try {
        const record = JSON.parse(readFileSync(full, "utf8"));
        out.push(receiptToImportPayload(record, full));
      } catch {
        // dual-read skips malformed
      }
    }
  }
  out.sort((a, b) => String(a.path).localeCompare(String(b.path)));
  return out;
}

export function buildImportCASRequest(dir, extraReceipts = []) {
  return {
    version: 1,
    id: `cas-import-${Date.now()}`,
    op: "import_cas_receipts",
    payload: {
      dir: dir || "",
      receipts: Array.isArray(extraReceipts) ? extraReceipts : [],
    },
  };
}
