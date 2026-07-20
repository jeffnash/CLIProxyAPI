import { parentPort } from "node:worker_threads";
import { sdkAgentGCRoots } from "./cursor-agent-bridge.mjs";

if (!parentPort) throw new Error("SDK agent root scanner must run in a worker thread");

parentPort.on("message", (message) => {
  const startedAt = Date.now();
  try {
    const roots = [...sdkAgentGCRoots()];
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
