import { parentPort } from "node:worker_threads";

import { runDurableFileMaintenance } from "./cursor-agent-bridge.mjs";

try {
  await runDurableFileMaintenance();
  parentPort?.postMessage({ ok: true });
} catch (error) {
  parentPort?.postMessage({ error: (error && error.stack) || (error && error.message) || String(error) });
  process.exitCode = 1;
}
