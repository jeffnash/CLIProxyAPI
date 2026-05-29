#!/usr/bin/env node
// Executes the bridge's fail-closed self-tests against the REAL patched @cursor/sdk bundle and exits
// nonzero on failure, so CI actually EXERCISES selfTestNativeUnreachable + selfTestBundleSeam (including
// the bundle-seam positive control) rather than only grepping for the patch markers. Requires the patched
// bundle to be installed (npm ci runs the postinstall patcher). Importing the bridge does not start the
// server (RUN_AS_MAIN is false here); we drive the self-tests directly, sequentially (they share globals).
import * as bridge from "../cursor-agent-bridge.mjs";

bridge.loadSdk(); // require + assert the patched bundle; this installs __CC_SELFTEST_DISPATCH_U/S
await bridge.selfTestNativeUnreachable();
await bridge.selfTestBundleSeam();
console.log("bridge self-tests passed (native unreachable + bundle seam)");
