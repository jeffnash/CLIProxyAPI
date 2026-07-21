import { parentPort } from "node:worker_threads";
import { sdkAgentGCCensus } from "./cursor-agent-bridge.mjs";

// Run the authoritative full census off the HTTP/SSE event loop. GC deliberately pays this
// cost before destructive checks: a secondary index that can miss an evidence commit is not
// safe deletion authority.

if (!parentPort) throw new Error("SDK agent root scanner must run in a worker thread");

parentPort.on("message", (message) => {
  const startedAt = Date.now();
  try {
    const roots = [...sdkAgentGCCensus()].sort();
    parentPort.postMessage({
      id: message?.id,
      roots,
      elapsedMs: Date.now() - startedAt,
    });
  } catch (error) {
    parentPort.postMessage({
      id: message?.id,
      error: (error && error.message) || String(error),
      elapsedMs: Date.now() - startedAt,
    });
  }
});
