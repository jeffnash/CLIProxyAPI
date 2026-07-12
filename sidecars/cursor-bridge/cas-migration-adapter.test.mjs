import assert from "node:assert/strict";
import { mkdtempSync, mkdirSync, writeFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import test from "node:test";
import {
  mapLegacyReceiptPhase,
  receiptToImportPayload,
  scanLegacyReceiptDir,
  buildImportCASRequest,
} from "./cas-migration-adapter.mjs";
import { ACCEPTANCE_PHASE } from "./acceptance-receipt.mjs";

test("mapLegacyReceiptPhase dual-reads delivering as maybe_accepted", () => {
  assert.equal(mapLegacyReceiptPhase({ status: "delivering" }), String(ACCEPTANCE_PHASE.MAYBE_ACCEPTED).toLowerCase());
  assert.equal(mapLegacyReceiptPhase({ acceptancePhase: "ACCEPTED" }), "accepted");
});

test("scanLegacyReceiptDir builds import payloads", () => {
  const root = mkdtempSync(path.join(tmpdir(), "cas-mig-"));
  try {
    mkdirSync(root, { recursive: true });
    writeFileSync(path.join(root, "a.json"), JSON.stringify({
      version: 5,
      status: "running",
      invocationId: "inv1_a",
      requestHash: "h",
      sessionId: "s",
      agentId: "ag",
      deliveryIdempotencyKey: "idem",
    }));
    const scanned = scanLegacyReceiptDir(root);
    assert.equal(scanned.length, 1);
    assert.equal(scanned[0].invocation_id, "inv1_a");
    assert.equal(scanned[0].phase, "accepted");
    const req = buildImportCASRequest(root);
    assert.equal(req.op, "import_cas_receipts");
    assert.equal(req.payload.dir, root);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("receiptToImportPayload prefers explicit acceptancePhase", () => {
  const payload = receiptToImportPayload({
    acceptancePhase: "completed",
    status: "failed",
    clientMessageId: "ccm",
  }, "/tmp/x.json");
  assert.equal(payload.phase, "completed");
  assert.equal(payload.invocation_id, "ccm");
});
