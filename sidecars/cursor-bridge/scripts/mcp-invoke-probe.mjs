#!/usr/bin/env node
// Local rapid test for the MCP-invocation fix (#75 / mcpStateExecArgs -> McpStateSuccess): drives ONE composer
// turn through a LOCALLY-running bridge that advertises a single MCP-shim-only tool, and reports whether
// composer-2.5 actually CALLS it. This replaces the slow Railway-redeploy loop — the ONLY external dependency
// is the Cursor API (the model runs there); the bridge + SDK runtime run locally and behave identically.
//
// Usage (two terminals):
//   A) start the bridge with your Cursor key + debug logs (it binds 127.0.0.1:9798):
//        CURSOR_API_KEY=crsr_... CURSOR_COMPOSER_DEBUG=1 node sidecars/cursor-bridge/cursor-agent-bridge.mjs
//   B) run this probe:
//        node sidecars/cursor-bridge/scripts/mcp-invoke-probe.mjs
//
// Watch terminal A for the DECISIVE markers:
//   [cct] mcp tools/call ... name=mcp__probe__echo   <- the model invoked the shim tool (FIX WORKS)
//   [cct] __CC_EXEC_U typed-unavailable ... cas=mcpStateExecArgs   <- MUST BE GONE after the fix
// The probe also parses the SSE stream and exits 0 iff a tool_call for the probe tool was observed.
//
// Env: CURSOR_AGENT_BRIDGE_URL (default http://127.0.0.1:9798), PROBE_MODEL (default composer-2.5),
//      PROBE_PROMPT (override the user text), PROBE_FORCE=1 (add tool_choice to force the tool — isolates
//      "model won't choose it" from "plumbing is broken"), CURSOR_AGENT_BRIDGE_TOKEN (multi-tenant auth).

const BRIDGE = process.env.CURSOR_AGENT_BRIDGE_URL || "http://127.0.0.1:9798";
const MODEL = process.env.PROBE_MODEL || "composer-2.5";
const TOOL = "mcp__probe__echo";
const PROMPT =
  process.env.PROBE_PROMPT ||
  `You have a tool named ${TOOL} that echoes text back. Call ${TOOL} with {"text":"ping"} right now. Do not answer in prose — you MUST call the tool to complete this.`;

const body = {
  sessionId: "mcp-probe-" + Date.now(),
  model: MODEL,
  input: { type: "user", text: PROMPT },
  // A single MCP-shim-ONLY tool (no native Cursor equivalent), so a call proves the model used the MCP path.
  tools: [
    {
      name: TOOL,
      description: "Echo the given text back to the caller. Use this whenever you are asked to echo text.",
      inputSchema: { type: "object", properties: { text: { type: "string" } }, required: ["text"] },
    },
  ],
};
if (process.env.PROBE_FORCE === "1") body.toolChoice = TOOL; // force via tool_choice: tests plumbing vs model-choice

const headers = { "content-type": "application/json" };
// Single-tenant: the bridge gates /agent/turn on `Authorization: Bearer <CURSOR_API_KEY>` (the Cursor key
// doubles as the bridge gate). Multi-tenant: X-Bridge-Auth gates access and the bearer is the per-user key.
// Forward whatever is in the env so the probe authenticates the same way CLIProxy does.
if (process.env.CURSOR_AGENT_BRIDGE_TOKEN) headers["X-Bridge-Auth"] = process.env.CURSOR_AGENT_BRIDGE_TOKEN;
if (process.env.CURSOR_API_KEY) headers["Authorization"] = "Bearer " + process.env.CURSOR_API_KEY;

async function main() {
  try {
    const h = await fetch(BRIDGE + "/health");
    if (!h.ok) throw new Error("health HTTP " + h.status);
  } catch (e) {
    console.error(
      `[probe] bridge not reachable at ${BRIDGE} (${(e && e.message) || e}). Start it first:\n` +
        `  CURSOR_API_KEY=crsr_... CURSOR_COMPOSER_DEBUG=1 node sidecars/cursor-bridge/cursor-agent-bridge.mjs`,
    );
    process.exit(2);
  }
  console.error(`[probe] POST ${BRIDGE}/agent/turn model=${MODEL} tool=${TOOL} force=${process.env.PROBE_FORCE === "1"}`);
  const res = await fetch(BRIDGE + "/agent/turn", { method: "POST", headers, body: JSON.stringify(body) });
  if (!res.ok) {
    const t = await res.text().catch(() => "");
    console.error(`[probe] /agent/turn HTTP ${res.status}: ${t.slice(0, 600)}`);
    if (res.status === 401 || res.status === 403) {
      console.error(`[probe] auth rejected — single-tenant bridges accept loopback by default; if yours is`);
      console.error(`[probe] multi-tenant, set CURSOR_AGENT_BRIDGE_TOKEN to the bridge's CURSOR_AGENT_BRIDGE_TOKEN.`);
    }
    process.exit(2);
  }
  const toolCalls = [];
  let textChars = 0;
  let turnEnd = null;
  const dec = new TextDecoder();
  let buf = "";
  for await (const chunk of res.body) {
    buf += dec.decode(chunk, { stream: true });
    let idx;
    while ((idx = buf.indexOf("\n")) >= 0) {
      const line = buf.slice(0, idx).trim();
      buf = buf.slice(idx + 1);
      if (!line.startsWith("data:")) continue;
      const payload = line.slice(5).trim();
      if (payload === "[DONE]") continue;
      let ev;
      try {
        ev = JSON.parse(payload);
      } catch {
        continue;
      }
      if (ev.type === "tool_call") {
        toolCalls.push(ev.name);
        console.error(`[probe] SSE tool_call: ${ev.name} (${ev.id})`);
      } else if (ev.type === "text") {
        textChars += String(ev.delta || "").length;
      } else if (ev.type === "turn_end") {
        turnEnd = ev;
        console.error(`[probe] SSE turn_end stop_reason=${ev.stop_reason}${ev.error ? " error=" + ev.error : ""}`);
      }
    }
  }
  const called = toolCalls.includes(TOOL);
  console.error("\n===== RESULT =====");
  console.error(`tool_calls observed : ${JSON.stringify(toolCalls)}`);
  console.error(`assistant text chars: ${textChars}`);
  if (called) {
    console.error(`✅ composer CALLED the MCP-shim tool ${TOOL} — mcpState fix unblocked MCP invocation.`);
    process.exit(0);
  }
  console.error(`❌ composer did NOT call ${TOOL} (it ${textChars ? "answered in prose" : "produced no tool call"}).`);
  console.error(`   In the BRIDGE log, check: is "[cct] mcp tools/call ... name=${TOOL}" present? Is`);
  console.error(`   "[cct] __CC_EXEC_U typed-unavailable ... cas=mcpStateExecArgs" GONE (it should be)?`);
  console.error(`   If the typed-unavailable line is gone but the tool still isn't called, re-run with`);
  console.error(`   PROBE_FORCE=1 — if it THEN calls the tool, the plumbing works and it's a model-choice issue.`);
  process.exit(1);
}

main().catch((e) => {
  console.error("[probe] error:", (e && e.message) || e);
  process.exit(2);
});
