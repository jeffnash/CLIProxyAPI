#!/usr/bin/env node
// Cursor Agent Bridge (Cursor Composer Client-Tools) — the official @cursor/sdk drives the Cursor agent, but EVERY tool
// executes on the end user's machine via Claude Code (through CLIProxy), and the sidecar filesystem is
// never touched for tool execution.
//
// TOPOLOGY (the sidecar ONLY talks to CLIProxy, never to the client directly):
//   Claude Code <-Anthropic /v1/messages-> CLIProxy (Go) <-HTTP/SSE /agent/turn-> THIS sidecar <-@cursor/sdk-> Cursor API
//
// Tools route to CC via the patched bundle's globalThis.__CC_EXEC_U/__CC_EXEC_S; the client's tools[]
// are advertised to the model as mcp_tools via globalThis.__CC_GET_ADVERTISE__ (patch inject). Results
// are built as Cursor protobuf messages by the patched serializeResult ($) doing <Type>.fromJson(ccJson).
//
// This revision incorporates the v2 adversarial audit's must-fixes:
//  - streaming discriminators are text-delta/thinking-delta (not text/thinking)
//  - sessionId is caller-supplied/minted (no per-turn content fingerprint; no cross-user collision)
//  - resume works: createAgent({agentId: sessionId}) so resumeAgent(sessionId) matches
//  - abort/cleanup: res 'close' rejects pendings + cancels the run; per-pending watchdog; idle session eviction
//  - no turnSettled deadlock: zero-match tool_results error out; the handler races settle vs res-close
//  - streamedText resets per user turn (no whole-turn re-emit)
//  - dispatchMcp reconciles the model's (often paraphrased) tool name against the advertised set
//  - control-flow exec cases (allowlist prechecks) return typed "allow", not a bare error
//  - flush timer is turn-scoped and cleared on settle (no cross-turn premature pause)
//  - real SIGTERM drain; startup mkdir + assert globals installed
//
// Env: CURSOR_API_KEY (required), CURSOR_AGENT_BRIDGE_PORT (default 9798),
//      CURSOR_AGENT_STATE_ROOT (default ./.cursor-agent-store — a writable volume on Railway),
//      CURSOR_AGENT_PENDING_TIMEOUT_MS (default 600000 in-process abandonment watchdog; NOT an upstream deadline),
//      CURSOR_AGENT_SESSION_TTL_MS (default 1800000 idle session eviction),
//      CURSOR_COMPOSER_MCP_SHIM (default ON; "0"/"false" disables registering the client's tools via the SDK's
//        official mcpServers path — the in-bridge /mcp streamable-http server),
//      CURSOR_COMPOSER_MCP_GROUPING (one|natural|per-tool, default natural — how advertised tools are
//        partitioned across the hosted MCP servers).

import { createServer } from "node:http";
import { randomUUID, timingSafeEqual, createHash } from "node:crypto";
import { fileURLToPath } from "node:url";
import { createRequire } from "node:module";
import { AsyncLocalStorage } from "node:async_hooks";
import { readFileSync, writeFileSync, mkdirSync, accessSync, constants, writeSync } from "node:fs";
import path from "node:path";

const PORT = parseInt(process.env.CURSOR_AGENT_BRIDGE_PORT || "9798", 10);
const API_KEY = process.env.CURSOR_API_KEY || "";
const STATE_ROOT = process.env.CURSOR_AGENT_STATE_ROOT || path.join(process.cwd(), ".cursor-agent-store");
const EMPTY_CWD = path.join(STATE_ROOT, ".empty");
const PENDING_TIMEOUT_MS = parseInt(process.env.CURSOR_AGENT_PENDING_TIMEOUT_MS || "600000", 10);
const SESSION_TTL_MS = parseInt(process.env.CURSOR_AGENT_SESSION_TTL_MS || "1800000", 10);
const MAX_SESSIONS = parseInt(process.env.CURSOR_AGENT_MAX_SESSIONS || "1000", 10);
const SSE_KEEPALIVE_MS = 15000;
// Tool-batch coalescing window. The @cursor/sdk never pauses for tools — it streams tool calls in waves —
// so this debounce merges a wave (emits <window apart) into ONE turn_end. It is a pure latency<->round-trip
// knob, NOT a correctness control: tools emitted after a turn closes are buffered + re-delivered next turn
// regardless (see emitToolUse/flushUndelivered), so any value is safe. Raise it (e.g. 150-200) to coalesce
// slower waves into fewer client round-trips at the cost of a little per-turn latency; lower it for snappier
// turns and more round-trips. Default 60 preserves the original behavior.
const TOOL_BATCH_MS = parseInt(process.env.CURSOR_AGENT_TOOL_BATCH_MS || "60", 10);
// Verbose per-turn diagnostic logging ([cct] lines) is OFF by default and gated behind this flag, so
// production logs stay clean and never echo request content. Set CURSOR_COMPOSER_DEBUG=1 to enable.
const COMPOSER_DEBUG = process.env.CURSOR_COMPOSER_DEBUG === "1" || process.env.CURSOR_COMPOSER_DEBUG === "true";
// MCP shim: register the client's advertised tools through the @cursor/sdk's OFFICIAL mcpServers path by
// hosting a tiny session-aware streamable-http MCP server inside this bridge (route /mcp/<sessionId>). This
// makes composer-2.5 actually CALL advertised tools (subagents/Agent, MCP tools, WebSearch, …) instead of
// only native read/shell. DEFAULT ON; disabled ONLY when the value is exactly "0" or "false" (any case).
// Fully fail-safe: when off (or on any build error) buildMcpServers returns {} and behavior is byte-for-byte
// today's native-only path. The /mcp route is dialed by the in-process SDK runtime over loopback only.
const MCP_SHIM_RAW = String(process.env.CURSOR_COMPOSER_MCP_SHIM ?? "").trim().toLowerCase();
const MCP_SHIM_ENABLED = !(MCP_SHIM_RAW === "0" || MCP_SHIM_RAW === "false");
// Grouping: how advertised tools are partitioned across MCP servers — one|natural|per-tool (default natural).
//   one      -> a single server "cc" advertising ALL tools (one MCP connection).
//   natural  -> reconstruct the user's real MCP topology from mcp__<server>__<tool> names; non-mcp tools
//               (Bash/Read/Task/WebSearch/…) lump into a synthetic "claude-code" server (cosmetic; bounds
//               each tools/list payload and mirrors the model's expected grouping).
//   per-tool -> one server per advertised tool (worst-case compat/diagnostic mode).
// Parsed once at startup; an unknown value falls back to "natural" with a console.warn.
const MCP_GROUPING = (() => {
  const g = String(process.env.CURSOR_COMPOSER_MCP_GROUPING ?? "").trim().toLowerCase();
  if (g === "" || g === "natural") return "natural";
  if (g === "one" || g === "per-tool") return g;
  console.warn(`[bridge] CURSOR_COMPOSER_MCP_GROUPING="${g}" is not one of one|natural|per-tool — falling back to "natural"`);
  return "natural";
})();
// Per-session FIFO queue depth: concurrent NEW-USER turns on one session are serialized (not rejected);
// this bounds how many may wait behind the active turn before a last-resort 429 (Layer A diverts tool-less
// one-shots, so reaching this requires many genuine concurrent agentic turns on one conversation).
const MAX_QUEUE_DEPTH = parseInt(process.env.CURSOR_AGENT_MAX_QUEUE_DEPTH || "8", 10);
// M26: cap how many bytes a single /agent/turn (and /mcp) request body may read into memory, so a buggy or
// hostile authenticated CLIProxy cannot OOM the sidecar (which would kill every session on this bridge). The
// cap is GENEROUS — large histories + base64 images are legitimate — defaulting to 64 MB and env-overridable
// via MAX_AGENT_TURN_BYTES; a real conversation must never hit it. Past the cap we stop concatenating and
// return 413 (a READ bound, not an upstream wall-clock timeout, so it is allowed by AGENTS.md).
const MAX_AGENT_TURN_BYTES = (() => {
  const n = parseInt(process.env.MAX_AGENT_TURN_BYTES || "", 10);
  return Number.isFinite(n) && n > 0 ? n : 64 * 1024 * 1024;
})();
// Shared SSE response headers (unbuffered, so keepalives reach the wire end-to-end).
const SSE_HEADERS = { "Content-Type": "text/event-stream", "Cache-Control": "no-cache", Connection: "keep-alive", "X-Accel-Buffering": "no" };
// Multi-tenant (opt-in): when CURSOR_AGENT_BRIDGE_TOKEN is set, X-Bridge-Auth gates access and the
// Authorization bearer is the PER-USER Cursor key (each gets an isolated SDK platform + stateRoot).
// When unset (default), behavior is single-tenant: the bearer must equal CURSOR_API_KEY and is the key.
const BRIDGE_TOKEN = process.env.CURSOR_AGENT_BRIDGE_TOKEN || "";
const MULTI_TENANT = BRIDGE_TOKEN !== "";
const MAX_PLATFORMS = parseInt(process.env.CURSOR_AGENT_MAX_PLATFORMS || "64", 10);
const PLATFORM_TTL_MS = parseInt(process.env.CURSOR_AGENT_PLATFORM_TTL_MS || "3600000", 10);
const RUN_AS_MAIN = process.argv[1] === fileURLToPath(import.meta.url);

// ---- load the PATCHED CJS bundle (NOT `import`, which resolves to unpatched dist/esm); assert patched ----
// Loading is lazy (loadSdk) so this module can be imported for unit tests without pulling the SDK's
// heavy/native deps; the real server calls loadSdk() at startup (fail-closed) BEFORE it accepts traffic.
const require = createRequire(import.meta.url);
function assertPatched(p) {
  if (!p.endsWith(path.join("dist", "cjs", "index.js"))) {
    throw new Error(`[bridge] @cursor/sdk resolved to ${p}, expected dist/cjs/index.js — refusing to start (tools would run natively on the sidecar FS).`);
  }
  if (!readFileSync(p, "latin1").slice(0, 64).includes("cursor-composer-clienttools-patched-v1")) {
    throw new Error(`[bridge] @cursor/sdk at ${p} is NOT patched (missing cursor-composer-clienttools-patched-v1). Run scripts/apply-clienttools-patch.cjs (reinstall a pristine bundle first if it was patched by an older version). Refusing to start.`);
  }
}
let _sdk = null;
function loadSdk() {
  if (_sdk) return _sdk;
  const p = require.resolve("@cursor/sdk");
  assertPatched(p);
  _sdk = require("@cursor/sdk");
  return _sdk;
}

// constEq is a constant-time, length-checked equality for secrets (false for empty).
function constEq(a, b) {
  const x = Buffer.from(String(a == null ? "" : a)), y = Buffer.from(String(b == null ? "" : b));
  return x.length === y.length && x.length > 0 && timingSafeEqual(x, y);
}

// authorizeRequest gates a /agent/turn request and returns the Cursor key to use for it, or "" if
// unauthorized. Single-tenant (default): the Authorization bearer must equal CURSOR_API_KEY and IS the
// Cursor key. Multi-tenant (CURSOR_AGENT_BRIDGE_TOKEN set): X-Bridge-Auth gates access (constant-time)
// and the Authorization bearer is the PER-USER Cursor key CLIProxy forwarded (each user thus runs under
// their own Cursor account + an isolated stateRoot); it falls back to CURSOR_API_KEY if none is forwarded.
// authorizeRequestWith is the pure core (testable without env): given the request headers and the bridge
// config, returns the Cursor key to use, or "" if unauthorized.
function authorizeRequestWith(headers, { apiKey, bridgeToken }) {
  const h = headers || {};
  const m = /^Bearer\s+(.+)$/i.exec(h.authorization || "");
  const bearer = m ? m[1] : "";
  if (bridgeToken) {
    if (!constEq(h["x-bridge-auth"], bridgeToken)) return "";
    return bearer || apiKey;
  }
  return constEq(bearer, apiKey) ? apiKey : "";
}
function authorizeRequest(req) {
  return authorizeRequestWith((req && req.headers) || {}, { apiKey: API_KEY, bridgeToken: BRIDGE_TOKEN });
}

// isConversationTooLong matches the Cursor "conversation too long" error class (BR-PL). When a run dies with
// this, the session is poisoned (every resume re-sends the same over-budget history); the caller drops the
// session so the NEXT turn re-seeds a fresh one. Matches the SDK's ERROR_CONVERSATION_TOO_LONG token plus a
// generic phrasing as a safety net (the exact upstream string may vary across Cursor releases).
function isConversationTooLong(msg) {
  return /ERROR_CONVERSATION_TOO_LONG|conversation (is )?too long/i.test(String(msg || ""));
}

// parseShellContent accepts either a plain stdout string or a JSON object the Go/CC side may send
// carrying a structured result {stdout, stderr, exitCode, aborted} so non-zero exits are not masked.
function parseShellContent(c) {
  if (c && typeof c === "object") {
    return { stdout: String(c.stdout ?? ""), stderr: String(c.stderr ?? ""), exitCode: Number(c.exitCode ?? c.exit_code ?? 0), aborted: Boolean(c.aborted) };
  }
  const s = String(c ?? "");
  if (s.startsWith("{")) {
    try { const o = JSON.parse(s); if (o && (("exitCode" in o) || ("exit_code" in o) || ("stdout" in o))) return parseShellContent(o); } catch { /* plain */ }
  }
  return { stdout: s, stderr: "", exitCode: 0, aborted: false };
}

// ccToolId derives the tool-call id used as BOTH the emitted SSE id and our pending-map key. It restricts
// the id to [a-zA-Z0-9_-] — the exact charset internal/util.SanitizeClaudeToolID allows — so the id the
// Claude client echoes back (after that sanitizer runs on the outbound leg) equals the key we store here;
// otherwise an id containing ':' '.' '=' or a space would be rewritten outbound and never match our pending
// call inbound (the tool result would be lost and the turn would hang/error). The fallback uses a FULL
// random uuid (not a truncated 8-hex slice) to avoid 32-bit birthday collisions across sessions.
function ccToolId(s) {
  const raw = (s && s.toolCallId) || `tc_${randomUUID()}`;
  return String(raw).replace(/[^a-zA-Z0-9_-]/g, "_");
}

// toSdkImages maps the bridge image shape to the SDK's SDKImage. Two shapes are accepted (C4):
//   - inline base64: {data, mimeType} — the SDK requires BOTH fields; emitted unchanged.
//   - URL: {url[, mimeType]} — http(s) images that are not base64; emitted as {url} (or {url, mimeType}).
// Each image is validated and a malformed one throws (failing the turn loudly) rather than silently sending
// an image the SDK would reject or mis-render. An entry with neither a valid data+mimeType NOR a valid url
// throws. (The executor already skips degenerate attachments upstream, so a throw here is a real bug.)
function toSdkImages(images) {
  if (!Array.isArray(images)) return [];
  return images.map((im, i) => {
    if (im && typeof im.url === "string" && im.url) {
      return typeof im.mimeType === "string" && im.mimeType ? { url: im.url, mimeType: im.mimeType } : { url: im.url };
    }
    if (!im || typeof im.data !== "string" || !im.data || typeof im.mimeType !== "string" || !im.mimeType) {
      throw new Error(`[bridge] image[${i}] is missing required data/mimeType or url (the @cursor/sdk image shape is {data, mimeType} or {url[, mimeType]})`);
    }
    return { data: im.data, mimeType: im.mimeType };
  });
}

// collectToolResultImages gathers any images carried INSIDE tool results (BR9/EX3: the executor extracts
// image parts from a role:tool message and threads them as tr.images). The Cursor tool-result protobuf
// shape cannot carry images, so they are folded into the C1/re-seed user send instead. Returns a flat array
// of image maps ({data,mimeType} or {url[,mimeType]}); empty when none are present.
function collectToolResultImages(input) {
  const out = [];
  for (const tr of (input && input.results) || []) {
    if (Array.isArray(tr.images)) for (const im of tr.images) if (im) out.push(im);
  }
  return out;
}

// constraintInstructions turns the OpenAI-style enforced constraints the SDK has no first-class params
// for (response_format / stop / token limit / tool_choice required|specific|none) into a model instruction
// block appended to the user turn, so the Cursor agent honors what the request asked for.
// IMPORTANT LIMIT (H08/H20/H21): these are MODEL INSTRUCTIONS, not hard protocol enforcement. The composer
// path cannot truly enforce stop sequences / token caps / response_format / parallel-call limits server-side
// (Cursor generates the tokens; the bridge only relays), and Cursor's NATIVE tools (read/shell/write/...) are
// SDK built-ins that cannot be structurally un-advertised — so tool_choice:none/specific gating of native
// tools is BEST-EFFORT (this instruction) PLUS a hard reject of native exec cases in the dispatch seam
// (see __CC_EXEC_U/S + nativeToolBlockedByChoice). `forcedUnavailable` (H09) is set when a forced
// specific:<name> tool is not in the advertised set: we tell the model it is unavailable rather than offering
// other tools or pretending the constraint held.
function constraintInstructions({ toolChoice, responseFormat, stop, maxTokens, forcedUnavailable } = {}) {
  const lines = [];
  const tc = toolChoice || "";
  if (forcedUnavailable && tc.startsWith("specific:")) {
    // H09: never widen to other tools; tell the model the forced tool cannot be used this turn.
    const nm = tc.slice("specific:".length);
    lines.push(`The tool "${nm}" was required for this request but is NOT available. Do not call any other tool as a substitute; explain that the requested tool is unavailable.`);
  } else if (tc === "required") {
    lines.push("You MUST call one of the available tools to fulfill this request; do not produce a final answer until you have called at least one tool.");
  } else if (tc.startsWith("specific:")) {
    const nm = tc.slice("specific:".length);
    lines.push(`You MUST call the tool named "${nm}" to fulfill this request, and you may call only that tool.`);
  } else if (tc === "none") {
    // H08 (best-effort): instruct the model to use NO tools, including the built-in file/shell tools that we
    // cannot un-advertise. The dispatch seam additionally hard-rejects any native exec case under `none`.
    lines.push("Do NOT call any tools for this request — neither the advertised tools nor any built-in file, shell, search, or edit tool. Produce your answer directly.");
  }
  if (responseFormat && typeof responseFormat === "object") {
    if (responseFormat.type === "json_object") {
      lines.push("Respond with a single valid JSON object only — no prose, no markdown code fences.");
    } else if (responseFormat.type === "json_schema") {
      const schema = (responseFormat.json_schema && (responseFormat.json_schema.schema || responseFormat.json_schema)) || {};
      lines.push("Respond with a single valid JSON value that conforms EXACTLY to this JSON Schema (no prose, no markdown code fences):\n" + JSON.stringify(schema));
    }
  }
  if (Array.isArray(stop) && stop.length) {
    lines.push("Stop your response immediately before emitting any of these sequences: " + stop.map((s) => JSON.stringify(s)).join(", ") + ".");
  }
  if (Number.isFinite(maxTokens) && maxTokens > 0) {
    lines.push(`Keep your entire response within approximately ${maxTokens} tokens.`);
  }
  return lines.join("\n");
}

// effectiveAdvertise restricts what tools the model SEES for a turn based on tool_choice:
// none -> none; specific:<name> -> just that tool; required/auto/unset -> the full advertised set.
// H09: a forced specific:<name> that does NOT match the advertised inventory must NOT widen to the full set
// (the old behavior silently let the model call an unrelated tool while the caller believed a single tool was
// forced). Instead advertise NOTHING for the missed forced tool; the caller (constraintInstructions) surfaces
// a model-visible "forced tool unavailable" instruction so the turn degrades honestly rather than mis-routing.
function effectiveAdvertise(advertise, toolChoice) {
  const adv = Array.isArray(advertise) ? advertise : [];
  const tc = toolChoice || "";
  if (tc === "none") return [];
  if (tc.startsWith("specific:")) {
    const nm = tc.slice("specific:".length);
    const only = adv.filter((t) => (t.toolName || t.name) === nm);
    return only; // H09: empty when the forced tool is not advertised (never widen to all)
  }
  return adv;
}

// forcedToolUnavailable reports whether a forced specific:<name> tool_choice cannot be satisfied by the
// advertised set (H09). When true the turn must tell the model the tool is unavailable instead of silently
// offering other tools or pretending the constraint held.
function forcedToolUnavailable(advertise, toolChoice) {
  const tc = toolChoice || "";
  if (!tc.startsWith("specific:")) return false;
  const nm = tc.slice("specific:".length);
  const adv = Array.isArray(advertise) ? advertise : [];
  return !adv.some((t) => (t.toolName || t.name) === nm);
}

// nativeToolBlockedByChoice (H08, BEST-EFFORT) reports whether a NATIVE Cursor tool (read/shell/write/...) is
// disallowed for the current turn given tool_choice. Native tools are SDK built-ins (not in the advertise set
// and not structurally un-advertisable), so we enforce the policy at the dispatch seam: under `none` no tool
// may run; under `specific:<name>` only that ADVERTISED client tool may run (native built-ins are never the
// forced advertised tool, so they are blocked); `auto`/`required`/unset leave native tools available. This is
// best-effort (the model is also instructed via constraintInstructions); it is NOT full structural gating.
function nativeToolBlockedByChoice(toolChoice) {
  const tc = toolChoice || "";
  if (tc === "none") return true;
  if (tc.startsWith("specific:")) return true;
  return false;
}

// ---- session correlation: the global executor learns its session via AsyncLocalStorage ----
const als = new AsyncLocalStorage(); // store = { session }
// The patch reads this to advertise the client's tools (incl MCPs) as mcp_tools, per-session.
globalThis.__CC_GET_ADVERTISE__ = () => {
  const st = als.getStore();
  if (!st || !st.session) { console.warn("[bridge] __CC_GET_ADVERTISE__: no ALS session context; advertising no tools"); return []; }
  const adv = st.session.advertise || [];
  // Proves the SDK's tool-advertising path (the non-prewarmed else branch) actually runs per model turn and
  // how many tools it hands the model. If this fires with a full count yet the model still calls only native
  // read/shell, the gap is the MODEL not engaging mcpTools — not a missing advertisement.
  dbg("__CC_GET_ADVERTISE__ called by SDK", "session=" + st.session.id, "returning=" + adv.length + " tools");
  return adv;
};

// Convert a proto Value/Struct/JSON-string into plain JSON (mcpArgs.args arrives as a proto map<string,Value>).
function toPlainJson(v) {
  if (v == null) return {};
  if (typeof v === "string") { try { return JSON.parse(v); } catch { return v; } }
  if (typeof v.toJson === "function") { try { return v.toJson(); } catch { /* fall through */ } }
  return v;
}

// Headless request context (never goes to CC): neutral /workspace paths, no sidecar dirs.
function headlessRequestContext(clientEnv) {
  const ce = clientEnv || {};
  const ws = Array.isArray(ce.workspacePaths) && ce.workspacePaths.length ? ce.workspacePaths : ["/workspace"];
  const cwd = ce.processWorkingDirectory || ws[0] || "/workspace";
  return { __ccJson: { success: { requestContext: {
    rules: [],
    env: { osVersion: ce.osVersion || "linux", workspacePaths: ws, shell: ce.shell || "bash", sandboxEnabled: false,
      terminalsFolder: cwd + "/.notes/terminals", agentSharedNotesFolder: cwd + "/.notes/shared",
      agentConversationNotesFolder: cwd + "/.notes/conv", timeZone: ce.timeZone || "UTC", projectFolder: cwd,
      agentTranscriptsFolder: cwd + "/.notes/transcripts", sandboxSupported: false,
      sandboxNetworkExplicitAllowlist: [], computerUseSupported: false, isWorkingDirHomeDir: false,
      processWorkingDirectory: cwd },
    repositoryInfo: [], tools: [], conversationNotesListing: "(none)", sharedNotesListing: "(none)",
    gitRepos: [], projectLayouts: [], mcpInstructions: [], fileContents: {}, customSubagents: [],
    commitAttributionMessage: "enabled", prAttributionMessage: "enabled", agentSkills: [],
    precomputedHumanChanges: [], supportsMcpAuth: true, gitRepoInfoComplete: true,
    mcpMetaToolOptions: { enabled: true, mcpDescriptors: [] }, nonFileRules: [] } } } };
}

// ── Coverage of all 29 ExecServerMessage cases (@cursor/sdk 1.0.14; verify via the bundle's
//    ExecServerMessage .fields.list() on any SDK bump). EVERY case is routed, synthesized, or rejected —
//    none falls through to native sidecar execution:
//   ROUTED→CC (CC_CASES):   readArgs, redactedReadArgs, writeArgs, deleteArgs, shellArgs, shellStreamArgs
//   ROUTED→CC (mcpArgs):    mcpArgs  (client/MCP tools, via dispatchMcp + reconcileToolName)
//   HEADLESS (synthetic):   requestContextArgs
//   CONTROL_ALLOW:          shellAllowlistPrecheckArgs, mcpAllowlistPrecheckArgs, webFetchAllowlistPrecheckArgs
//   CONTROL_TYPED rejected: diagnosticsArgs, canvasDiagnosticsArgs
//   FAIL-CLOSED reject:     grepArgs, lsArgs (model uses shell instead — structured shapes TODO),
//                           backgroundShellSpawnArgs, forceBackgroundShellArgs, writeShellStdinArgs,
//                           executeHookArgs, subagentArgs, subagentAwaitArgs, forceBackgroundSubagentArgs,
//                           fetchArgs, recordScreenArgs (no GUI), computerUseArgs (no GUI),
//                           listMcpResourcesExecArgs, readMcpResourceExecArgs, mcpStateExecArgs,
//                           smartModeClassifierArgs (TODO: typed default-mode success, live-validate)
//
// ccErrorMessageText renders an isError result's content into a single human-readable failure message for
// the `error: {message}` variant. The Go side may thread the failure reason as a plain string or a small
// structured object; either way the model must SEE the failure (C01), never a fabricated success shape.
function ccErrorMessageText(c, fallback) {
  if (typeof c === "string" && c.trim()) return c;
  if (c && typeof c === "object") { try { return JSON.stringify(c); } catch { /* fall through */ } }
  return fallback;
}

// Native Cursor tool cases routed to CC. ccTool = generic name (CLIProxy maps to the client's exact tool
// + arg schema). buildResult/buildChunks turn CC's tool_result content into the Cursor result toJson shape.
// C01/BR8: every buildResult takes (content, state, isError). When the client marked the tool result FAILED
// (isError), it MUST build a FAILURE shape — the result type's `error` oneof variant (agent.v1.Error =
// {message}) — so a failed/cancelled/denied read/write/delete reaches the model AS a failure instead of a
// fabricated native success (which would let the model continue from a false filesystem state). ReadResult /
// WriteResult / DeleteResult each expose a `result` oneof with `success` plus `error`/`rejected`/typed error
// variants (verified against @cursor/sdk 1.0.14 proto descriptors); the patched `fromJson` builds whichever
// oneof key we emit. The shell cases already encode failure via a non-zero exitCode (their success variant is
// the protocol's failure channel), so they keep their existing exit-code handling.
const CC_CASES = {
  readArgs:        { ccTool: "read",  stream: false, buildResult: (c, s, isError) => isError ? { error: { message: ccErrorMessageText(c, "read failed") } } : ({ success: { path: s && s.path, content: String(c ?? ""), totalLines: String(c ?? "").split("\n").length, fileSize: String(Buffer.byteLength(String(c ?? ""))), truncated: false, rangeApplied: false } }) },
  redactedReadArgs:{ ccTool: "read",  stream: false, buildResult: (c, s, isError) => isError ? { error: { message: ccErrorMessageText(c, "read failed") } } : ({ success: { path: s && s.path, content: String(c ?? ""), totalLines: String(c ?? "").split("\n").length, fileSize: String(Buffer.byteLength(String(c ?? ""))), truncated: false, rangeApplied: false } }) },
  writeArgs:       { ccTool: "write", stream: false, buildResult: (c, s, isError) => { if (isError) return { error: { message: ccErrorMessageText(c, "write failed") } }; const t = (s && s.fileText) || ""; const r = { success: { path: s && s.path, linesCreated: t.split("\n").length, fileSize: String(Buffer.byteLength(t)) } }; if (s && s.returnFileContentAfterWrite) r.success.fileContentAfterWrite = t; return r; } },
  deleteArgs:      { ccTool: "delete", stream: false, buildResult: (c, s, isError) => isError ? { error: { message: ccErrorMessageText(c, "delete failed") } } : ({ success: { path: s && s.path, deletedFile: true, fileSize: "0" } }) },
  // BR8/C5: when the client marked the native shell result as failed (isError) but the parsed exitCode is 0,
  // force a non-zero exit so the model sees the failure (a success exit would mask a failed/cancelled tool).
  shellArgs:       { ccTool: "shell", stream: false, buildResult: (c, s, isError) => { const r = parseShellContent(c); const code = isError && r.exitCode === 0 ? 1 : r.exitCode; return { success: { command: s && s.command, workingDirectory: "/workspace", exitCode: code, stdout: r.stdout, stderr: r.stderr } }; } },
  shellStreamArgs: { ccTool: "shell", stream: true,  buildChunks: (c, isError) => { const r = parseShellContent(c); const code = isError && r.exitCode === 0 ? 1 : r.exitCode; const aborted = isError ? true : r.aborted; const out = [{ stdout: { data: r.stdout } }]; if (r.stderr) out.push({ stderr: { data: r.stderr } }); out.push({ exit: { code, cwd: "/workspace", aborted, localExecutionTimeMs: 1 } }); return out; } },
  // grep/ls have complex structured results (workspace_results / directory_tree_root); v1 leaves them
  // fail-closed (rejected) and the model uses the shell tool (rg/ls). TODO: implement structured shapes.
  grepArgs:        { ccTool: "grep", stream: false, buildResult: null },
  lsArgs:          { ccTool: "ls",   stream: false, buildResult: null },
};
// Control-flow exec cases the server may send: answer with a typed "allow" so the run proceeds (a bare
// error reject can deny the action / desync). allowlisted is a bool.
const CONTROL_ALLOW = { shellAllowlistPrecheckArgs: 1, mcpAllowlistPrecheckArgs: 1, webFetchAllowlistPrecheckArgs: 1 };
// Server-proactive cases that may fire at turn start: answer with a benign TYPED result so the run
// proceeds — a bare Error throw is a plausible desync/ERROR_BAD_REQUEST vector. If a shape is wrong,
// fromJson throws and it degrades to the same exec error (no worse than rejecting).
// diagnosticsArgs/canvasDiagnosticsArgs -> typed "rejected" (DiagnosticsResult has a rejected variant).
// TODO(validate-live): smartModeClassifierArgs needs its success shape (default mode) + subagent* a typed
// error; left as deny-by-default reject until their shapes are derived and exercised against live Cursor.
const CONTROL_TYPED = { diagnosticsArgs: { rejected: {} }, canvasDiagnosticsArgs: { rejected: {} } };
// H17: cases the bridge does NOT implement but whose RESULT type exposes an `error` oneof variant
// (agent.v1.Error{message}, verified against the @cursor/sdk 1.0.14 proto descriptors). For these we return a
// MODEL-VISIBLE typed unavailable result instead of fail-closing the whole run with a stream error — so the
// model sees "this tool is unavailable" and picks another path (e.g. shell instead of structured grep/ls).
// CAVEAT (H17): this is ONLY for cases with a known typed-error result shape. Cases with NO safe result
// variant (subagent*/forceBackgroundSubagent/recordScreen/computerUse/smartModeClassifier/executeHook, and the
// streaming-lifecycle force-background-shell) are deliberately LEFT fail-closed below — fabricating a result
// there could desync the run; the correct degrade for those is to not let the model rely on them. Each case
// maps to its <X>Result via the SDK's <X>Args -> <X>Result convention; if a mapping were wrong, fromJson
// throws and it degrades to the same exec error as the old reject (no worse).
const TYPED_UNAVAILABLE_U = new Set([
  "grepArgs",                 // GrepResult.error                 (model falls back to shell rg)
  "lsArgs",                   // LsResult.error                   (model falls back to shell ls)
  "fetchArgs",                // FetchResult.error
  "backgroundShellSpawnArgs", // BackgroundShellSpawnResult.error
  "writeShellStdinArgs",      // WriteShellStdinResult.error
  "listMcpResourcesExecArgs", // ListMcpResourcesExecResult.error
  "readMcpResourceExecArgs",  // ReadMcpResourceExecResult.error
  "mcpStateExecArgs",         // McpStateExecResult.error
]);
function typedUnavailableResult(cas) {
  return { __ccJson: { error: { message: `tool '${cas}' is not available in this environment; use an alternative approach` } } };
}

function ccArgsFor(cas, s) {
  switch (cas) {
    case "readArgs": case "redactedReadArgs": return { path: s && s.path, offset: s && s.offset, limit: s && s.limit };
    case "shellArgs": case "shellStreamArgs": return { command: s && s.command, cwd: (s && s.workingDirectory) || undefined };
    case "writeArgs": return { path: s && s.path, content: s && s.fileText };
    case "deleteArgs": return { path: s && s.path };
    default: return s;
  }
}

// blockedNativeResult builds a typed FAILURE result for a native tool the model tried to use while tool_choice
// disallowed it (H08). It uses the SAME failure channels as a real failed tool — the `error` variant for
// read/write/delete and a non-zero/aborted exit for shell — so the model SEES the block and chooses another
// path, never a fabricated success. Returns the unary { __ccJson } shape; the streaming case yields chunks.
const NATIVE_TOOL_DISABLED_MSG = "tool disabled for this turn by tool_choice policy";
function blockedNativeResult(cas, s) {
  switch (cas) {
    case "shellArgs":
      return { __ccJson: { success: { command: s && s.command, workingDirectory: "/workspace", exitCode: 1, stdout: "", stderr: NATIVE_TOOL_DISABLED_MSG } } };
    default:
      // read/write/delete/redactedRead -> the result type's `error` oneof variant (agent.v1.Error{message}).
      return { __ccJson: { error: { message: NATIVE_TOOL_DISABLED_MSG } } };
  }
}
function caseOf(t) { return t && t.message && t.message.case; }

// ---- the patched bundle calls these (deny-by-default; never native) ----
globalThis.__CC_EXEC_U = function (n, e, s, t) {
  const cas = caseOf(t);
  const store = als.getStore();
  if (cas === "requestContextArgs") return Promise.resolve(headlessRequestContext(store && store.session && store.session.clientEnv));
  if (CONTROL_ALLOW[cas]) return Promise.resolve({ __ccJson: { allowlisted: true } });
  if (CONTROL_TYPED[cas]) return Promise.resolve({ __ccJson: CONTROL_TYPED[cas] });
  // H17: cases we don't implement but whose result type has a typed `error` variant -> a MODEL-VISIBLE typed
  // unavailable result (the model picks another path) instead of fail-closing the whole run. Checked BEFORE
  // the CC_CASES reject so grep/ls (which carry buildResult:null) also degrade to a typed result rather than a
  // stream error. (Subagent/computerUse/etc. without a safe result shape still fall through to the reject.)
  if (TYPED_UNAVAILABLE_U.has(cas)) {
    dbg("__CC_EXEC_U typed-unavailable result (H17)", "session=" + (store && store.session && store.session.id), "cas=" + cas);
    return Promise.resolve(typedUnavailableResult(cas));
  }
  if (cas === "mcpArgs") {
    if (!store) return Promise.reject(new Error("[bridge] mcpArgs outside a session"));
    return store.session.dispatchMcp(s);
  }
  const spec = CC_CASES[cas];
  if (!spec || spec.stream || !spec.buildResult || !store) {
    return Promise.reject(new Error(`[bridge] tool '${cas}' not supported by the Claude Code bridge`));
  }
  // H08 (best-effort): tool_choice none/specific must gate NATIVE built-in tools too. Return a typed FAILURE
  // result (model-visible) instead of executing the native tool on the client — never a fabricated success.
  if (nativeToolBlockedByChoice(store.session.toolChoice)) {
    dbg("__CC_EXEC_U native tool blocked by tool_choice", "session=" + store.session.id, "cas=" + cas, "toolChoice=" + store.session.toolChoice);
    return Promise.resolve(blockedNativeResult(cas, s));
  }
  return store.session.dispatchUnary(cas, spec, s);
};
globalThis.__CC_EXEC_S = function (n, e, s, t) {
  const cas = caseOf(t);
  const spec = CC_CASES[cas];
  const store = als.getStore();
  if (!spec || !spec.stream || !spec.buildChunks || !store) {
    return (async function* () { throw new Error(`[bridge] streaming tool '${cas}' not supported by the Claude Code bridge`); })();
  }
  // H08 (best-effort): block a native STREAMING tool (shellStream) under none/specific with a typed aborted
  // exit chunk so the model sees the failure instead of the tool running on the client.
  if (nativeToolBlockedByChoice(store.session.toolChoice)) {
    dbg("__CC_EXEC_S native tool blocked by tool_choice", "session=" + store.session.id, "cas=" + cas, "toolChoice=" + store.session.toolChoice);
    return (async function* () { yield { __ccJson: { stdout: { data: "" } } }; yield { __ccJson: { exit: { code: 1, cwd: "/workspace", aborted: true, localExecutionTimeMs: 1 } } }; })();
  }
  return store.session.dispatchStream(cas, spec, s);
};

// ---- session: holds the live SDK run + bridges tool calls across /agent/turn calls ----
const sessions = new Map();
// Bound the sessions map (no unbounded growth): evict least-recently-active, non-streaming sessions over the cap.
function enforceSessionCap() {
  if (sessions.size <= MAX_SESSIONS) return;
  const evictable = [...sessions.values()].filter((s) => !s.activeRes && !s.run && !s.hasQueuedWaiters()).sort((a, b) => a.lastActivity - b.lastActivity);
  for (const s of evictable) { if (sessions.size <= MAX_SESSIONS) break; sessions.delete(s.id); void s.cancel(); }
}
// Per-Cursor-key platform pool. Single-tenant: one entry keyed by API_KEY with stateRoot = STATE_ROOT
// (NOT namespaced, so existing durable sessions survive an upgrade). Multi-tenant: one platform per
// forwarded key, each with an isolated stateRoot STATE_ROOT/k_<hash>, so distinct users never share a
// Cursor account or durable state. Bounded (MAX_PLATFORMS) + idle-evicted so the pool can't grow without limit.
const platforms = new Map(); // keyHash -> { promise, stateRoot, lastUsed }
function keyHash(k) { return createHash("sha256").update(String(k || "")).digest("hex").slice(0, 16); }
function platformStateRoot(h) { return MULTI_TENANT ? path.join(STATE_ROOT, "k_" + h) : STATE_ROOT; }
function getPlatform(cursorKey) {
  const h = keyHash(cursorKey);
  let entry = platforms.get(h);
  if (!entry) {
    const stateRoot = platformStateRoot(h);
    try { mkdirSync(stateRoot, { recursive: true }); } catch { /* createAgentPlatform will surface a real error */ }
    entry = { promise: loadSdk().createAgentPlatform({ apiKey: cursorKey, stateRoot }), stateRoot, lastUsed: nowMs() };
    platforms.set(h, entry);
    enforcePlatformCap();
  }
  entry.lastUsed = nowMs();
  return entry.promise;
}
async function disposePlatform(entry) {
  try {
    const p = await entry.promise;
    if (p && p.store && p.store.dispose) await p.store.dispose();
    if (p && p.checkpointStore && p.checkpointStore.dispose) await p.checkpointStore.dispose();
    if (p && p.eventStore && p.eventStore.dispose) await p.eventStore.dispose();
  } catch { /* best-effort */ }
}
// A platform is PINNED (not evictable) while ANY session references its key — disposing it would close the
// sqlite stores out from under a session that is paused awaiting tool_results (its SDK run is still live,
// just between HTTP turns, so activeRes is null). Both the cap and the idle timer use this ONE predicate so
// they never diverge — the original bug was the cap checking only activeRes while the idle timer didn't.
function platformHasSession(h, sess = sessions) {
  for (const s of sess.values()) {
    if (keyHash(s.cursorKey) === h) return true;
  }
  return false;
}
// Evict the least-recently-used platforms over the cap, skipping any still backing a tracked session.
function enforcePlatformCap() {
  if (platforms.size <= MAX_PLATFORMS) return;
  const sorted = [...platforms.entries()].sort((a, b) => a[1].lastUsed - b[1].lastUsed);
  for (const [h, entry] of sorted) {
    if (platforms.size <= MAX_PLATFORMS) break;
    if (platformHasSession(h)) continue;
    platforms.delete(h);
    void disposePlatform(entry);
  }
}

class Session {
  constructor(id, cursorKey) {
    this.id = id;
    // C05: the DURABLE Cursor agent id is DECOUPLED from the external sessionID. `id` is the stable routing
    // key the Go executor derives (continuations keep routing here); `agentId` is what we hand to
    // resumeAgent/createAgent. They start equal, but on ERROR_CONVERSATION_TOO_LONG we ROTATE `agentId`
    // (e.g. <id>_r2) and tombstone the poisoned durable agent, so the next turn seeds a FRESH agent under a
    // new id and never resumeAgent()s the over-budget one — while the external sessionID is unchanged.
    this.agentId = id;
    this.recoveryEpoch = 0;       // C05: increments each too-long rotation; suffixes the rotated agentId
    this.cursorKey = cursorKey || API_KEY; // the Cursor key whose platform this session runs on
    this.agent = null; this.agentPromise = null; this.run = null;
    this.activeRes = null; this.pending = new Map();
    this.turnBatch = []; this.flushTimer = null;
    this.delivered = new Set();   // tool ids the client has SEEN (sent in a turn_end) this logical run — so a
                                  // tool_results turn matches against what was actually delivered (Comment 2)
    this.everEmitted = new Set(); // BR1: EVERY tool id this session has ever issued to the client, across the
                                  // whole session lifetime (NOT cleared per run like `delivered`). A late
                                  // tool_result for an id we DID emit (then watchdog-reaped / already resolved)
                                  // is benign; only an id NEVER in this set is genuinely unknown -> error turn.
    this.undelivered = [];        // {id,name,input} of tools emitted with no open response (turn closed mid-burst);
                                  // delivered on the next /agent/turn so the client can answer them (Comment 1)
    this.rawToWireId = new Map();  // H23: raw SDK tool-call id -> the sanitized WIRE id we emitted for it. Same
                                   // raw id always maps to the same wire id (idempotent); two DIFFERENT raw ids
                                   // that sanitize to the same wire id get DISAMBIGUATED so neither overwrites
                                   // the other's pending (the original collision bug at pending.set).
    this.usedWireIds = new Set();  // H23: every wire id this session has handed out (collision detection set).
    this.turnToken = 0;           // increments per turn; flush is bound to a token
    this.settleTurn = null;
    this.streamedText = "";       // cumulative text streamed in the CURRENT run (reset per user turn)
    this.advertise = [];
    this.seeded = false;          // first user send done? system + history are prepended only on the first send
    this.seededSystem = "";       // C3/BR6: the system prompt last seeded to the model; a continuation that
                                  // carries a DIFFERENT system (e.g. ExitPlanMode) re-applies it on the next send
    this.historyFingerprint = null; // C2/BR-C2: fingerprint of the inbound non-system history last seen; a
                                  // changed fingerprint on an established session (e.g. /compact) forces a re-seed
    this.reseeding = false;       // C2/BR-C2 transient: a forced re-seed is prepending the REPLACED history,
                                  // so the BR-DS "resume found prior turns -> seeded" probe must not suppress it
    this.lastRunError = null;     // BR2: last upstream/run error message; used so a paused run that died upstream
                                  // surfaces as a turn_end{error} on the next tool_results instead of false success
    this.clientEnv = null;        // client's real env (workspace/cwd/shell) for headless requestContext
    this.toolChoice = "";         // H08: the current turn's resolved tool_choice token (auto|none|required|specific:<n>);
                                  // read by the dispatch seam to best-effort gate NATIVE tools. Set per send in driveUserSend.
    this.lastActivity = nowMs();
    this.done = false;
    this.tail = Promise.resolve();   // per-session FIFO chain: each new-user turn runs after the prior one's run completes
    this.waiters = 0;                // new-user turns queued but not yet running (single source of truth for depth-cap + eviction safety)
    this._logicalDone = [];          // resolvers fired when the live run TRULY completes (onRunComplete/onRunError/cancel), NOT at a tool-pause
    this.runEpoch = 0;               // bumped per run + on cancel; a run.wait() callback ignores its result if the epoch advanced (the run was superseded/cancelled and a new turn may already own the session)
  }

  touch() { this.lastActivity = nowMs(); }
  hasQueuedWaiters() { return this.waiters > 0; }
  // whenLogicalDone resolves when the CURRENT live run terminates. If no run is live, it resolves now. This
  // is the queue's admission signal and is deliberately DISTINCT from settle(): settle() also fires when a
  // turn pauses for client tools while the SDK run stays alive awaiting tool_results, so admitting the next
  // turn on settle() would collide with the still-live run. Admission must wait for real completion.
  whenLogicalDone() { if (!this.run) return Promise.resolve(); return new Promise((r) => this._logicalDone.push(r)); }
  notifyLogicalDone() { const ws = this._logicalDone; this._logicalDone = []; for (const w of ws) { try { w(); } catch {} } }
  sse(obj) { if (this.activeRes) { try { this.activeRes.write(`data: ${JSON.stringify(obj)}\n\n`); } catch { /* ignore */ } } }

  // wireToolId derives the per-session WIRE tool-call id from the SDK tool spec, resolving sanitization
  // collisions (H23). ccToolId sanitizes the raw id to the Claude charset; two distinct raw ids (e.g. "call:a"
  // and "call_a", or "x.y" and "x_y") can sanitize to the SAME wire id, and the second pending.set would then
  // overwrite the first — one tool result resolving the wrong pending, the other hanging. We keep a per-session
  // raw->wire map (idempotent: the same raw id always yields the same wire id, so a re-emit is stable) plus a
  // used-wire-id set; on a collision with a DIFFERENT raw id we append a short stable hash of the raw id so the
  // wire id is unique. A spec with no toolCallId mints a fresh tc_<uuid> (already collision-free) and is not
  // tracked in the map (each mint is unique).
  wireToolId(s) {
    const raw = (s && s.toolCallId) || null;
    if (!raw) return ccToolId(s); // minted uuid path: unique by construction, no collision tracking needed
    const existing = this.rawToWireId.get(raw);
    if (existing) return existing; // same raw id -> same wire id (idempotent re-emit)
    let wire = ccToolId(s);
    if (this.usedWireIds.has(wire)) {
      // Collision: a DIFFERENT raw id already owns this sanitized wire id. Disambiguate with a short stable
      // hash of the raw id, retrying (extremely unlikely) until unique, so neither pending is overwritten.
      const base = wire;
      let n = 0;
      do {
        const h = createHash("sha256").update(raw + "#" + n).digest("hex").slice(0, 8);
        wire = `${base}_${h}`;
        n++;
      } while (this.usedWireIds.has(wire));
      dbg("wireToolId collision disambiguated", "session=" + this.id, "raw=" + raw, "wire=" + wire);
    }
    this.rawToWireId.set(raw, wire);
    this.usedWireIds.add(wire);
    return wire;
  }

  newPending(id, resolveWrap) {
    const timer = setTimeout(() => {
      const p = this.pending.get(id);
      if (p) { this.pending.delete(id); p.reject(new Error(`[bridge] tool ${id} abandoned after ${PENDING_TIMEOUT_MS}ms`)); }
    }, PENDING_TIMEOUT_MS);
    this.pending.set(id, { resolve: resolveWrap, reject: (err) => { clearTimeout(timer); resolveWrap.__reject(err); }, timer });
  }
  // resolvePending answers a pending client tool call. isError (C5/BR4) flags a FAILED/cancelled client tool
  // so the result reaches the model AS a failure (the resolve wrappers in dispatchUnary/Stream/Mcp route it
  // into the Cursor result's isError / non-zero exit shapes) instead of being reported as a clean success.
  resolvePending(id, content, isError = false) {
    const p = this.pending.get(id);
    if (!p) return false;
    clearTimeout(p.timer); this.pending.delete(id); p.resolve(content, isError === true); return true;
  }

  // matchToolResults is the ONE shared strict tool-result matcher used by BOTH the normal runTurn path AND
  // the concurrent activeRes fast path (C-TOOLRESULT-MATCH). Having a single matcher is the fix for C04 (the
  // concurrent path used to clean-ack with no unknown/zero-match safety, faking success). For each result it
  // applies, in order:
  //   1. resolve-by-id against session.pending (threading isError) -> matched.
  //   2. else if the id was EVER emitted/delivered by this session -> a BENIGN ack (watchdog-reaped or already
  //      resolved); NOT unknown.
  //   3. else -> unknown (an id this session NEVER issued: a genuine desync/foreign id).
  // C03: the old unconditional `pending.size === 1` fallback is REMOVED — a non-empty toolCallId is matched
  // STRICTLY. The ONLY idless escape is an EXPLICIT `tr.idless === true` flag a schema translator sets when it
  // can PROVE the client carried no id (e.g. a future Gemini path with no functionCall.id); absent the flag,
  // we never guess. With C02 (Gemini now emits real functionCall.id) the parallel-Gemini case round-trips by
  // id, so the heuristic fallback is no longer needed.
  // Returns { matched:int, unknown:string[] } (pending count is read off session.pending by the caller).
  matchToolResults(results) {
    let matched = 0;
    const unknown = [];
    for (const tr of results || []) {
      const isErr = tr.isError === true;
      if (this.resolvePending(tr.toolCallId, tr.content, isErr)) { matched++; continue; }
      // Explicit idless/minted result: a translator proved there was no client-visible id. Resolve the lone
      // pending ONLY when exactly one is outstanding (never guess among several). This is the sole survivor of
      // the removed C03 fallback and fires only behind the explicit flag.
      if (tr.idless === true && this.pending.size === 1) {
        const loneId = this.pending.keys().next().value;
        dbg("matchToolResults idless 1-pending resolve", "session=" + this.id, "resolving=" + loneId);
        if (this.resolvePending(loneId, tr.content, isErr)) { matched++; continue; }
      }
      // BR1: an id that misses but was ever emitted/delivered is benign (reaped or already resolved). Only an
      // id never issued by this session is genuinely unknown.
      if (!this.delivered.has(tr.toolCallId) && !this.everEmitted.has(tr.toolCallId)) unknown.push(tr.toolCallId);
    }
    return { matched, unknown };
  }

  dispatchUnary(cas, spec, s) {
    const id = this.wireToolId(s); // H23: collision-safe per-session wire id
    return new Promise((resolve, reject) => {
      // C5/BR8: buildResult sees isError so a native tool the client marked failed (e.g. shell) is routed
      // through the failure shape (non-zero exitCode) instead of being reported to the model as success.
      const wrap = (content, isError) => { try { resolve({ __ccJson: spec.buildResult(content, s, isError === true) }); } catch (err) { reject(err); } };
      wrap.__reject = reject;
      this.newPending(id, wrap);
      this.emitToolUse(id, spec.ccTool, ccArgsFor(cas, s));
    });
  }
  dispatchStream(cas, spec, s) {
    const id = this.wireToolId(s); // H23: collision-safe per-session wire id
    const self = this;
    return (async function* () {
      // C5/BR8: carry isError alongside the streamed content so buildChunks can emit a non-zero exit chunk.
      const { content, isError } = await new Promise((resolve, reject) => {
        const wrap = (c, e) => resolve({ content: c, isError: e === true }); wrap.__reject = reject;
        self.newPending(id, wrap);
        self.emitToolUse(id, spec.ccTool, ccArgsFor(cas, s));
      });
      for (const chunk of spec.buildChunks(content, isError)) yield { __ccJson: chunk };
    })();
  }
  // The model called one of the client's advertised tools (incl MCPs). Reconcile the (often paraphrased)
  // name against the advertised set, then route to CC. CC's text result becomes the McpResult content.
  dispatchMcp(s) {
    const id = this.wireToolId(s); // H23: collision-safe per-session wire id
    const want = (s && (s.toolName || s.name)) || "";
    const ccName = this.reconcileToolName(want);
    const input = toPlainJson(s && s.args);
    // Every tool the MODEL actually calls lands here (raw name + whether it reconciled to an advertised tool).
    // This is how we tell whether composer ever invokes a harness tool like Task/Agent (subagent spawn) and,
    // if it does, whether the call survives reconciliation or is rejected as "not available".
    dbg("dispatchMcp", "session=" + this.id, "want=" + want, "reconciled=" + (ccName || "<UNAVAILABLE>"));
    if (!ccName) {
      const names = (this.advertise || []).map((t) => t.toolName || t.name).join(", ");
      dbg("dispatchMcp TOOL NOT AVAILABLE", "want=" + want, "advertisedCount=" + (this.advertise || []).length, "advertised=" + names);
      return Promise.resolve({ __ccJson: { success: { isError: true, content: [{ text: { text: `Tool '${want}' is not available. Available tools: ${names || "(none)"}.` } }] } } });
    }
    return new Promise((resolve, reject) => {
      // C5/BR4: a client tool that failed/was cancelled (isError) must reach the model AS a failure, so the
      // McpResult's isError mirrors the threaded flag rather than being hardcoded false.
      const wrap = (content, isError) => resolve({ __ccJson: { success: { isError: isError === true, content: [{ text: { text: typeof content === "string" ? content : JSON.stringify(content ?? "") } }] } } });
      wrap.__reject = reject;
      this.newPending(id, wrap);
      this.emitToolUse(id, ccName, input);
    });
  }
  reconcileToolName(want) {
    const adv = this.advertise || [];
    if (!adv.length) return null;
    const names = adv.map((t) => t.toolName || t.name);
    if (names.includes(want)) return want;                              // exact
    const lw = (want || "").toLowerCase();
    const ci = names.filter((nm) => nm.toLowerCase() === lw);           // case-insensitive
    if (ci.length === 1) return ci[0];
    const tok = (s) => String(s || "").toLowerCase().split(/[-_.:/ ]+/).filter(Boolean);
    // H18: the single-advertised-tool rule (the model slightly misnaming the ONLY tool) is INTENTIONAL but is
    // now GUARDED — apply it ONLY when `want` is a PLAUSIBLE variant of that one tool, NEVER for an arbitrary
    // unrelated/known-foreign id (e.g. routing `nanobanana_generate` to the only tool `Bash` would turn a
    // hallucinated action into a real call). Plausible = case/punctuation-insensitive equal, OR token overlap
    // with the one tool's name. If it does not plausibly match -> null (caller returns a typed isError
    // "unavailable" result), so a foreign id is rejected rather than routed to a powerful tool.
    if (adv.length === 1) {
      const only = names[0];
      const norm = (s) => String(s || "").toLowerCase().replace(/[^a-z0-9]/g, "");
      if (norm(want) === norm(only)) return only;                      // punctuation/case variant of the one tool
      const wt = tok(lw), ot = new Set(tok(only));
      if (wt.some((t) => ot.has(t))) return only;                      // shares a token with the one tool
      dbg("reconcileToolName single-tool guard rejected implausible name", "session=" + this.id, "want=" + want, "only=" + only);
      return null;                                                     // unrelated/foreign -> typed isError
    }
    // token-boundary fuzzy: accept ONLY when exactly one advertised tool shares the want's last token.
    // (No substring includes() — that mis-routed e.g. "config" -> "reconfigure_database".)
    const tail = tok(lw).pop() || lw;
    const matches = names.filter((nm) => tok(nm).includes(tail));
    return matches.length === 1 ? matches[0] : null;                   // ambiguous/none -> typed isError
  }

  emitToolUse(id, name, input) {
    this.touch();
    this.everEmitted.add(id); // BR1: record EVERY issued id (lifetime), so a late result is benign not "unknown"
    if (!this.activeRes) {
      // No open client-facing response — the prior turn already closed (e.g. the debounce flushed mid-burst
      // and the SDK kept emitting). Writing the tool_call to a dead socket would silently create a pending
      // the client can never answer (the desync). Buffer it and deliver it on the next /agent/turn (Comment 1).
      dbg("emitToolUse BUFFERED (no activeRes)", "session=" + this.id, "id=" + id, "name=" + name);
      this.undelivered.push({ id, name, input });
      return;
    }
    dbg("emitToolUse", "session=" + this.id, "id=" + id, "name=" + name);
    this.turnBatch.push({ id, name, input });
    this.sse({ type: "tool_call", id, name, input });
    const token = this.turnToken;
    if (this.flushTimer) clearTimeout(this.flushTimer);
    this.flushTimer = setTimeout(() => { if (token === this.turnToken) this.pauseForTools(); }, TOOL_BATCH_MS);
  }
  // flushUndelivered delivers tools that were emitted while no response was open, on a later turn's OPEN
  // response, so the client finally sees them and can answer them. Emits one tool_use turn_end + settles.
  flushUndelivered() {
    if (!this.undelivered.length || !this.activeRes) return false;
    const batch = this.undelivered;
    this.undelivered = [];
    dbg("flushUndelivered", "session=" + this.id, "count=" + batch.length, "ids=" + safeJson(batch.map((t) => t.id)));
    for (const t of batch) { this.delivered.add(t.id); this.everEmitted.add(t.id); this.sse({ type: "tool_call", id: t.id, name: t.name, input: t.input }); }
    this.sse({ type: "turn_end", stop_reason: "tool_use", tool_calls: batch.map((t) => t.id) });
    this.settle();
    return true;
  }
  pauseForTools() {
    this.flushTimer = null;
    for (const b of this.turnBatch) this.delivered.add(b.id);
    this.sse({ type: "turn_end", stop_reason: "tool_use", tool_calls: this.turnBatch.map((b) => b.id) });
    this.turnBatch = [];
    this.settle();
  }
  settle() { const f = this.settleTurn; this.settleTurn = null; if (this.flushTimer) { clearTimeout(this.flushTimer); this.flushTimer = null; } if (f) f(); }

  onRunComplete(res) {
    if (this.done) return;
    this.done = true; this.run = null;
    // BR2: a non-"finished" terminal means the upstream run failed; remember it so a tool_results turn that
    // finds nothing to resume surfaces the real error instead of a false-success empty turn.
    if (res && res.status !== "finished") this.lastRunError = (res && res.error) || "run did not finish";
    dbg("onRunComplete", "session=" + this.id, "status=" + (res && res.status), "error=" + safeJson(res && res.error),
      "streamedTextLen=" + this.streamedText.length, "resultLen=" + ((res && res.result) || "").length);
    // text-delta deltas already streamed the full text incrementally. Only fall back to the
    // res.result lump if NO deltas fired this run (non-streaming edge) — otherwise we'd duplicate.
    if (!this.streamedText) { const full = (res && res.result) || ""; if (full) this.sse({ type: "text", delta: full }); }
    this.sse({ type: "turn_end", stop_reason: res && res.status === "finished" ? "end_turn" : "error", status: res && res.status, error: res && res.error, usage: (res && res.usage) || {} });
    this.rejectAllPending("run completed");
    this.clearTurnState();
    this.settle();
    this.notifyLogicalDone(); // real completion -> admit the next queued new-user turn
  }
  onRunError(err) {
    if (this.done) return;
    this.done = true; this.run = null;
    const msg = (err && err.message) || String(err);
    this.lastRunError = msg; // BR2: a tool_results turn that finds the run gone surfaces this real error
    dbg("onRunError", "session=" + this.id, (err && err.stack) || msg);
    this.sse({ type: "turn_end", stop_reason: "error", error: msg });
    this.rejectAllPending("run errored");
    this.clearTurnState();
    this.settle();
    this.notifyLogicalDone(); // run terminated (error) -> admit the next queued new-user turn
    // C05/BR-PL: a conversation-too-long failure poisons the DURABLE Cursor agent — every resume of it hits
    // the same over-budget wall. Decouple recovery from the external sessionID: ROTATE the durable agentId
    // (tombstone the poisoned one, allocate <id>_r2/_r3/...), force seeded=false + drop the fingerprint, and
    // KEEP the in-memory session (so the same external sessionID still routes here). The NEXT turn then seeds
    // a FRESH agent from the client's bounded history and NEVER calls resumeAgent(oldAgentId). The error turn
    // was already surfaced above (no false success). Bounded rotations avoid unbounded agentId churn if even
    // the bounded history is too large; past the cap we stop rotating and the user keeps seeing the real error.
    if (isConversationTooLong(msg)) void this.rotateDurableAgent();
  }
  // rotateDurableAgent tombstones the poisoned durable agent and allocates a fresh agentId (C05). The session
  // (external id) is intentionally KEPT in the map so continuations keep routing here; only the durable
  // agentId the bridge hands to resume/create changes. seeded/seededSystem/historyFingerprint are reset so
  // the next turn re-seeds the client's bounded history into the fresh agent.
  async rotateDurableAgent() {
    const COMPOSER_MAX_RECOVERY_ROTATIONS = 3;
    if (this.recoveryEpoch >= COMPOSER_MAX_RECOVERY_ROTATIONS) {
      // Past the cap: stop rotating (avoid unbounded churn) but DROP the session so the next turn starts a
      // clean in-memory session against the last rotated agentId. Never fake success — the error turn already fired.
      dbg("rotateDurableAgent cap reached -> drop session", "session=" + this.id, "agentId=" + this.agentId, "epoch=" + this.recoveryEpoch);
      sessions.delete(this.id);
      await this.cancel();
      return;
    }
    // Set the rotation state SYNCHRONOUSLY (before any await) so the rotated agentId / reset seeded are
    // observable the moment onRunError returns — the next turn must not race a half-applied rotation. The
    // async teardown (cancel) follows; only `done` flips after it (a fresh send re-opens the session).
    const oldAgentId = this.agentId;
    this.recoveryEpoch++;
    this.agentId = `${this.id}_r${this.recoveryEpoch + 1}`; // first rotation -> <id>_r2
    this.seeded = false;          // re-seed the bounded history into the fresh agent
    this.seededSystem = "";
    this.historyFingerprint = null;
    dbg("rotateDurableAgent CONVERSATION_TOO_LONG -> rotate durable agentId (no resumeAgent(old))", "session=" + this.id, "old=" + oldAgentId, "new=" + this.agentId);
    // Tear down the poisoned live agent/run WITHOUT deleting the session. cancel() nulls agent/run + rejects
    // pendings; we re-open the session for the next turn afterwards (done=false) so it can re-seed.
    await this.cancel();
    this.done = false;            // cancel() set done=true; the next turn must be able to drive a fresh send
  }
  rejectAllPending(why) {
    for (const [, p] of this.pending) { try { p.reject(new Error(`[bridge] ${why}`)); } catch {} }
    this.pending.clear();
  }
  // Clear per-run tool-delivery state when the logical run ends/errors/cancels (Comment 1): stale turnBatch,
  // undelivered buffer, and the delivered set must not leak into the next logical run on this session.
  clearTurnState() {
    if (this.flushTimer) { clearTimeout(this.flushTimer); this.flushTimer = null; }
    this.turnBatch = []; this.undelivered = []; this.delivered.clear();
  }
  async cancel() {
    this.done = true;     // short-circuit any late run.wait() settlement (onRunComplete/onRunError no-op on done)
    this.runEpoch++;      // invalidate the in-flight run's completion callback so it can't mutate a successor turn
    this.rejectAllPending("session cancelled");
    this.clearTurnState();
    try { await (this.run && this.run.cancel && this.run.cancel()); } catch {}
    try { await (this.agent && this.agent.close && this.agent.close()); } catch {}
    this.run = null;
    // Null the closed agent handle so a surviving queued waiter (the session is kept when waiters remain)
    // re-resumes/recreates a live agent via ensureAgent instead of reusing this dead one.
    this.agent = null; this.agentPromise = null;
    this.settle();
    this.notifyLogicalDone(); // run torn down -> release any queued waiter so the chain advances
  }
}

function nowMs() { return Date.now(); }

// ── H12: durable per-agent history fingerprint (survives a bridge restart) ──────────────────────────────
// The BR-DS optimization (resume a durable agent that has prior turns -> mark seeded, skip re-prepend) is
// correct for a NORMAL restart, but after a /compact the durable agent holds the OLD un-compacted body while
// the client now sends the COMPACTED history; resuming the durable agent silently keeps stale (over-budget)
// context. A cold in-memory session has no fingerprint to compare against, so we PERSIST the last seeded
// fingerprint to STATE_ROOT keyed by (key,agentId). On a cold resume we compare the inbound fingerprint to
// the durable one: if they DIFFER, the retained history was rewritten (a compact) -> re-seed instead of
// trusting the durable agent. Best-effort: any FS error degrades to BR-DS (trust durable). This is a bounded
// read/write of a tiny 32-hex file, NOT a network timeout (allowed under AGENTS.md). The full-tree-rewrite
// race where a restart coincides with the very first compacted turn AND no prior durable fp was ever written
// remains a flagged limitation (the executor's rewrite-sensitive fingerprint covers the multi-turn case).
const FP_DIR = path.join(STATE_ROOT, ".cct-fp");
function fpPathFor(cursorKey, agentId) {
  const safe = String(agentId || "").replace(/[^a-zA-Z0-9_.-]/g, "_").slice(0, 200);
  return path.join(FP_DIR, keyHash(cursorKey) + "_" + safe);
}
function readDurableFingerprint(cursorKey, agentId) {
  try { return readFileSync(fpPathFor(cursorKey, agentId), "utf8").trim() || null; } catch { return null; }
}
function writeDurableFingerprint(cursorKey, agentId, fp) {
  if (!fp) return;
  try { mkdirSync(FP_DIR, { recursive: true }); writeFileSync(fpPathFor(cursorKey, agentId), String(fp), "utf8"); }
  catch (e) { dbg("writeDurableFingerprint failed (best-effort)", "agentId=" + agentId, (e && e.message) || String(e)); }
}

async function ensureAgent(session, model) {
  if (session.agent) return session.agent;
  if (session.agentPromise) return session.agentPromise;          // guard TOCTOU
  session.agentPromise = (async () => {
    const platform = await getPlatform(session.cursorKey);
    const opts = { model: { id: model }, apiKey: session.cursorKey, local: { cwd: EMPTY_CWD } };
    // MCP shim registration (additive, never substitutive): attach the session's MCP server map so the SDK's
    // local runtime dials our in-bridge /mcp/<id> endpoint and surfaces the advertised tools to the model.
    // Wrapped so any throw degrades to today's native-only behavior — the working read/shell path MUST survive
    // any shim failure. Built per ensureAgent so a session whose tools change across runs re-registers correctly,
    // and applied to BOTH the resume and create branches below (they spread the same opts).
    try {
      const servers = buildMcpServers(session);
      if (servers && Object.keys(servers).length) opts.mcpServers = servers;
    } catch (e) {
      dbg("ensureAgent buildMcpServers failed (continuing native-only)", "session=" + session.id, (e && e.message) || String(e));
    }
    // C05: resume/create against the DURABLE agentId (rotates on too-long), not the external session id.
    const agentId = session.agentId || session.id;
    dbg("ensureAgent resumeAgent", "session=" + session.id, "agentId=" + agentId, "model=" + model, "mcpServers=" + (opts.mcpServers ? Object.keys(opts.mcpServers).length : 0));
    try {
      session.agent = await platform.resumeAgent(agentId, opts);          // cold / restart: resume by our durable agentId
      // BR-DS: a successful resume of a DURABLE agent that already has prior turns (e.g. after a bridge
      // restart, when this in-memory session has not seeded yet) means the SDK already holds the running
      // conversation. Mark seeded so the next send does NOT re-prepend the entire history on top of it
      // (which would double the context and risk ERROR_CONVERSATION_TOO_LONG). Best-effort + defensive: if
      // the SDK exposes no message probe (or it throws), leave seeded as-is — never guess. `reseeding` is set
      // by runTurn when a /compact (incl. H12 cold-restart compact) must re-prepend the rewritten history;
      // this probe respects it and does NOT mark seeded then.
      if (!session.seeded && !session.reseeding && typeof platform.getAgentMessages === "function") {
        try {
          const prior = await platform.getAgentMessages(agentId, { limit: 1 });
          if (Array.isArray(prior) && prior.length > 0) {
            session.seeded = true;
            dbg("ensureAgent resume found prior turns -> seeded=true (no re-prepend)", "session=" + session.id, "priorCount>=" + prior.length);
          }
        } catch (probeErr) {
          dbg("ensureAgent getAgentMessages probe failed (leaving seeded as-is)", "session=" + session.id, (probeErr && probeErr.message) || String(probeErr));
        }
      }
    } catch (err) {
      // Only create-on-not-found. A transient resume error (model resolution / network) must NOT
      // fall through to createAgent (which PK-collides on an existing agent id) — rethrow so CLIProxy retries.
      const msg = (err && err.message) || String(err);
      dbg("ensureAgent resumeAgent FAILED", "session=" + session.id, "agentId=" + agentId, msg);
      if (!/not found/i.test(msg)) { dbg("ensureAgent rethrow (not 'not found')", "session=" + session.id); throw err; }
      dbg("ensureAgent createAgent (was not found)", "session=" + session.id, "agentId=" + agentId);
      session.agent = await platform.createAgent({ agentId, ...opts });
    }
    return session.agent;
  })();
  try { return await session.agentPromise; } finally { session.agentPromise = null; }
}

// ──────────────────────────── MCP shim (in-bridge streamable-http MCP server) ────────────────────────────
// The SDK's local runtime is a real MCP client: given an http McpServerConfig it connects out, calls
// tools/list, surfaces those tools to composer-2.5, and drives tools/call when the model picks one. We host
// that server here over loopback (route /mcp/<sessionId>[/<serverKey>]) so a tools/call converges on the
// SAME pending/emit machinery as a native dispatchMcp — the model's call becomes an SSE tool_call the client
// answers on a later /agent/turn (resolvePending fulfills the awaiting promise).

// mcpServerKeyForTool maps an advertised tool NAME to its server key under the active grouping. Pure helper.
//   one      -> always "cc".
//   natural  -> mcp__<server>__<tool> -> sanitize(<server>); everything else -> "claude-code".
//   per-tool -> sanitize(<toolName>) (one server per tool).
// sanitize restricts a key to a URL-safe segment [A-Za-z0-9_.-] (other chars -> "-").
function mcpSanitizeKey(s) { return String(s || "").replace(/[^A-Za-z0-9_.-]/g, "-"); }
function mcpServerKeyForTool(name, grouping = MCP_GROUPING) {
  const n = String(name || "");
  if (grouping === "one") return "cc";
  if (grouping === "per-tool") return mcpSanitizeKey(n);
  // natural: reconstruct the originating MCP server from the mcp__<server>__<tool> convention. Non-greedy
  // first group so the FIRST "__" after the prefix delimits server vs tool (a server token may itself carry
  // single underscores, e.g. plugin_chrome-devtools-mcp_chrome-devtools, but never "__").
  const m = n.match(/^mcp__(.+?)__(.+)$/);
  return m ? mcpSanitizeKey(m[1]) : "claude-code";
}

// mcpToolsForServer returns the slice of the session's advertised tools that belongs to serverKey under the
// active grouping. For grouping "one" (serverKey "cc" / empty) it returns ALL advertised tools. Recomputed
// per request (never cached) because session.advertise can change per turn. Each entry is shaped for
// tools/list: {name, description, inputSchema} with a valid object inputSchema default.
function mcpToolsForServer(session, serverKey, grouping = MCP_GROUPING) {
  const adv = (session && session.advertise) || [];
  const all = grouping === "one" || !serverKey || serverKey === "cc";
  const out = [];
  for (const t of adv) {
    const name = t.toolName || t.name;
    if (!name) continue;
    if (!all && mcpServerKeyForTool(name, grouping) !== serverKey) continue;
    const schema = t.inputSchema && typeof t.inputSchema === "object" ? t.inputSchema : { type: "object" };
    out.push({ name, description: t.description || "", inputSchema: schema });
  }
  return out;
}

// buildMcpServers returns the Record<serverKey, McpServerConfig> registered via AgentOptions.mcpServers for a
// session, or {} when the shim is off (DEFAULT ON; off only when CURSOR_COMPOSER_MCP_SHIM is "0"/"false").
// Every server is the same loopback http shape; only the SET of keys + the per-key tool slice differ by
// grouping. The url carries the sessionId (authoritative) and, when grouping != "one", the serverKey segment;
// a belt-and-suspenders X-CC-Session header is sent too (our handler ignores it — the path is authoritative).
// MUST be fail-safe: any throw returns {} so a shim bug can never break the working native path. R5: under
// "natural", if two distinct server tokens sanitize to the SAME key, degrade this session to "one" (correct
// full tool names are unchanged regardless) and log a dbg line.
function buildMcpServers(session) {
  try {
    if (!MCP_SHIM_ENABLED) return {};
    const adv = (session && session.advertise) || [];
    if (!adv.length) return {};
    const sid = session.id;
    const mkServer = (serverKey) => ({
      type: "http",
      url: `http://127.0.0.1:${PORT}/mcp/${sid}` + (serverKey && serverKey !== "cc" ? `/${serverKey}` : ""),
      headers: { "X-CC-Session": sid },
    });
    if (MCP_GROUPING === "one") return { cc: mkServer("cc") };
    const servers = {};
    // R5 collision guard (natural only): detect two distinct server tokens collapsing to one URL-safe key.
    const seenRaw = new Map(); // sanitizedKey -> rawServerToken (for the collision check)
    for (const t of adv) {
      const name = t.toolName || t.name;
      if (!name) continue;
      const key = mcpServerKeyForTool(name, MCP_GROUPING);
      if (MCP_GROUPING === "natural") {
        const m = name.match(/^mcp__(.+?)__(.+)$/);
        const raw = m ? m[1] : "claude-code";
        const prev = seenRaw.get(key);
        if (prev !== undefined && prev !== raw) {
          dbg("buildMcpServers natural key collision -> degrade to one", "session=" + sid, "key=" + key, "a=" + prev, "b=" + raw);
          return { cc: mkServer("cc") };
        }
        seenRaw.set(key, raw);
      }
      if (!servers[key]) servers[key] = mkServer(key);
    }
    return servers;
  } catch (e) {
    dbg("buildMcpServers threw (returning {})", "session=" + (session && session.id), (e && e.message) || String(e));
    return {};
  }
}

// mcpError builds a JSON-RPC 2.0 error response object (never thrown to the socket).
function mcpError(id, code, message) { return { jsonrpc: "2.0", id: id ?? null, error: { code, message } }; }
// mcpResult builds a JSON-RPC 2.0 success response object.
function mcpResult(id, result) { return { jsonrpc: "2.0", id: id ?? null, result }; }

// mcpDispatch handles ONE JSON-RPC message for the in-bridge MCP server and returns either a JSON-RPC
// response object, or null for a notification (no id) that needs only a 202. NEVER throws — every path is
// wrapped so the socket always receives a valid JSON-RPC object (fail-soft). sessionId + serverKey come from
// the URL path; the session is looked up in the existing `sessions` Map (no new session concept).
async function mcpDispatch(msg, sessionId, serverKey) {
  try {
    if (!msg || typeof msg !== "object" || msg.jsonrpc !== "2.0" || typeof msg.method !== "string") {
      return mcpError(msg && msg.id, -32600, "Invalid Request");
    }
    const { id, method, params } = msg;
    const hasId = id !== undefined && id !== null;
    switch (method) {
      case "initialize": {
        const ver = (params && typeof params.protocolVersion === "string" && params.protocolVersion) || "2025-06-18";
        dbg("mcp initialize", "session=" + sessionId, "serverKey=" + (serverKey || "cc"), "protocol=" + ver);
        return mcpResult(id, { protocolVersion: ver, capabilities: { tools: {} }, serverInfo: { name: "cursor-composer-clienttools", version: "1" } });
      }
      case "notifications/initialized":
        // A notification (no id): no state to track beyond the existing Session. The caller replies 202.
        return null;
      case "ping":
        return mcpResult(id, {});
      case "tools/list": {
        const session = sessions.get(sessionId);
        const tools = session ? mcpToolsForServer(session, serverKey) : [];
        dbg("mcp tools/list", "session=" + sessionId, "serverKey=" + (serverKey || "cc"), "count=" + tools.length);
        // Return everything; omit nextCursor (a few hundred tools is fine, no real pagination needed).
        return mcpResult(id, { tools });
      }
      case "tools/call": {
        const want = (params && params.name) || "";
        const input = (params && params.arguments) || {};
        const session = sessions.get(sessionId);
        dbg("mcp tools/call", "session=" + sessionId, "serverKey=" + (serverKey || "cc"), "name=" + want);
        if (!session) {
          // Degrade, never fake success: an unknown/expired session yields a typed isError tool result.
          return mcpResult(id, { content: [{ type: "text", text: `session ${sessionId} not found (bridge restart or idle eviction); the tool call cannot be routed` }], isError: true });
        }
        const ccName = session.reconcileToolName(want);
        dbg("mcp tools/call reconciled", "session=" + sessionId, "want=" + want, "reconciled=" + (ccName || "<UNAVAILABLE>"));
        if (!ccName) {
          const names = (session.advertise || []).map((t) => t.toolName || t.name).join(", ");
          return mcpResult(id, { content: [{ type: "text", text: `Tool '${want}' is not available. Available tools: ${names || "(none)"}.` }], isError: true });
        }
        // Correlate + await exactly like Session.dispatchMcp: ccToolId(undefined) mints a sanitized tc_<uuid>
        // that is BOTH the SSE tool_call id and the pending-map key. The only bound on the wait is the existing
        // PENDING_TIMEOUT_MS watchdog (no new data-path timeout). resolvePending (on the later tool_results
        // turn) fulfills `wrap`; rejectAllPending (run completed/errored/cancelled/abandoned) -> __reject.
        const callId = ccToolId(undefined);
        try {
          const content = await new Promise((resolve, reject) => {
            const wrap = (c) => resolve(c);
            wrap.__reject = reject;
            session.newPending(callId, wrap);
            session.emitToolUse(callId, ccName, input);
          });
          return mcpResult(id, { content: [{ type: "text", text: typeof content === "string" ? content : JSON.stringify(content ?? "") }], isError: false });
        } catch (rejErr) {
          // Run completed/errored/cancelled/abandoned before the client answered: a typed failure (per MCP,
          // tool-execution failures are RESULTS with isError, not protocol errors), so the runtime never hangs.
          return mcpResult(id, { content: [{ type: "text", text: (rejErr && rejErr.message) || String(rejErr) }], isError: true });
        }
      }
      default:
        // Unknown method: a JSON-RPC error for a request; silently drop an unknown notification (no id).
        return hasId ? mcpError(id, -32601, "Method not found") : null;
    }
  } catch (e) {
    // Last-resort fail-soft: never throw to the socket. A request gets a JSON-RPC error; a notification gets 202.
    dbg("mcpDispatch internal error", "session=" + sessionId, (e && e.message) || String(e));
    return msg && (msg.id !== undefined && msg.id !== null) ? mcpError(msg.id, -32603, "Internal error") : null;
  }
}

// handleMcp serves the /mcp/<sessionId>[/<serverKey>] streamable-http endpoint. POST carries a single
// JSON-RPC 2.0 message or a batch array; we always reply application/json with the JSON-RPC response (or 202
// for a pure-notification request). A GET (the optional server->client SSE channel we don't need) -> 405. The
// body is read the same way /agent/turn reads its body. NEVER throws to the socket.
async function handleMcp(req, res, sessionId, serverKey) {
  res.setHeader("Access-Control-Allow-Origin", "*");
  if (req.method !== "POST") {
    // streamable-http permits omitting the optional GET SSE stream (no server-initiated notifications here).
    res.writeHead(405, { Allow: "POST", "Content-Type": "application/json" });
    res.end(JSON.stringify(mcpError(null, -32600, "Method Not Allowed (POST only)")));
    return;
  }
  let raw;
  try { raw = await readBodyBounded(req); } // M26 (completeness): bound the /mcp body read too
  catch (e) {
    if (e instanceof PayloadTooLargeError) {
      dbg("handleMcp -> 413 body too large", "session=" + sessionId, e.message);
      res.writeHead(413, { "Content-Type": "application/json" });
      res.end(JSON.stringify(mcpError(null, -32600, e.message)));
      return;
    }
    dbg("handleMcp -> body read error", "session=" + sessionId, (e && e.message) || String(e));
    res.writeHead(400, { "Content-Type": "application/json" });
    res.end(JSON.stringify(mcpError(null, -32700, "Parse error")));
    return;
  }
  let body;
  try { body = JSON.parse(raw); } catch (e) {
    dbg("handleMcp -> -32700 parse error", "session=" + sessionId, (e && e.message) || String(e));
    res.writeHead(200, { "Content-Type": "application/json" });
    res.end(JSON.stringify(mcpError(null, -32700, "Parse error")));
    return;
  }
  // Always issue an Mcp-Session-Id (R4: liberal in, conservative out — the StreamableHTTP transport stores it
  // on initialize). We accept any/none on later requests; the URL path is the authority.
  res.setHeader("Mcp-Session-Id", sessionId);
  if (Array.isArray(body)) {
    const out = [];
    for (const m of body) { const r = await mcpDispatch(m, sessionId, serverKey); if (r) out.push(r); }
    // A batch of pure notifications yields no responses -> 202; otherwise return the response array.
    if (!out.length) { res.writeHead(202, { "Content-Type": "application/json" }); res.end(); return; }
    res.writeHead(200, { "Content-Type": "application/json" }); res.end(JSON.stringify(out)); return;
  }
  const result = await mcpDispatch(body, sessionId, serverKey);
  if (result === null) { res.writeHead(202, { "Content-Type": "application/json" }); res.end(); return; }
  res.writeHead(200, { "Content-Type": "application/json" }); res.end(JSON.stringify(result));
}

function streamCallbacks(session) {
  return {
    onDelta: ({ update }) => {
      try {
        const ty = update && (update.type || update.case);
        const txt = update && (update.text != null ? update.text : (update.value && update.value.text));
        if (ty === "text-delta" && txt) { session.streamedText += txt; session.sse({ type: "text", delta: txt }); }
        else if (ty === "thinking-delta" && txt) session.sse({ type: "reasoning", delta: txt });
      } catch (e) { dbg("onDelta ERROR", "session=" + session.id, (e && e.message) || String(e)); }
    },
    onStep: () => {},
  };
}

// ---- HTTP ----
// dbg writes a GUARANTEED-FLUSHED line to stdout (fd 1) so the sidecar's operational logs reach Railway
// even though Node block-buffers pipe stdout. Lines are content-free (session ids, statuses, lengths,
// error messages) — turn routing decisions and failures only, never request/response bodies.
function safeJson(a) { try { return typeof a === "string" ? a : JSON.stringify(a); } catch { return String(a); } }
function dbg(...args) { if (!COMPOSER_DEBUG) return; try { writeSync(1, "[cct] " + args.map(safeJson).join(" ") + "\n"); } catch { /* never throw from logging */ } }

// PayloadTooLargeError marks a request body that exceeded MAX_AGENT_TURN_BYTES so callers can map it to 413.
class PayloadTooLargeError extends Error { constructor(msg) { super(msg); this.code = "PAYLOAD_TOO_LARGE"; } }
// readBodyBounded reads a request body but stops + throws once the accumulated BYTE length exceeds
// MAX_AGENT_TURN_BYTES (M26), so a runaway body can never be fully buffered into memory. Byte length (not
// char length) is tracked so multibyte/base64 payloads are bounded accurately. Returns the raw string.
async function readBodyBounded(req) {
  let raw = "";
  let bytes = 0;
  for await (const c of req) {
    bytes += Buffer.byteLength(c);
    if (bytes > MAX_AGENT_TURN_BYTES) {
      throw new PayloadTooLargeError(`request body exceeds MAX_AGENT_TURN_BYTES (${MAX_AGENT_TURN_BYTES} bytes)`);
    }
    raw += c;
  }
  return raw;
}

async function handleTurn(req, res, body, cursorKey) {
  const input = body.input || (body.text != null ? { type: "user", text: body.text } : { type: "user", text: "" });
  const model = body.model || "composer-2.5";
  const sessionId = body.sessionId;
  const fail = (code, msg) => {
    dbg("handleTurn FAIL", code, "session=" + sessionId, "inputType=" + (input && input.type), msg);
    res.writeHead(code, { "Content-Type": "application/json" }); res.end(JSON.stringify({ error: msg }));
  };
  // Validate BEFORE opening the SSE so we can return a real HTTP status.
  if (!sessionId) { fail(400, "sessionId is required"); return; }

  // Enforced response constraints + tool_choice carried from the Go executor (Comment 3). Applied as
  // model instructions and tool-advertisement gating on the user turn.
  const constraints = { toolChoice: body.toolChoice || "", responseFormat: body.responseFormat, stop: body.stop, maxTokens: body.maxTokens };

  if (input.type === "tool_results") {
    // A continuation COMPLETES the active (paused) run; it must reach resolvePending promptly and must
    // NEVER queue behind a new-user turn (that would hang the run until the abandonment watchdog).
    const session = sessions.get(sessionId);
    if (!session) {
      // Unknown/expired session for a tool_results continuation (bridge restart or TTL eviction): the pending
      // tool calls cannot be reconstructed, so the continuation is genuinely LOST. Comment 1: do NOT complete
      // this as a turn at all. Emitting it as SSE over HTTP 200 — even with stop_reason:"error" — risks being
      // consumed as a clean terminal (the Go executor synthesizes a [DONE] terminal on any clean stream end),
      // silently discarding the client's tool work as a success. The tool result was NOT applied to a pending
      // run, so we must not return success. Headers are NOT written yet on this path, so return a real HTTP
      // error: the Go executor rejects any non-2xx /agent/turn response at its status check, BEFORE it parses
      // or synthesizes a terminal for the stream, surfacing the lost continuation as a hard error in BOTH the
      // streaming and non-streaming paths (rather than relying on a downstream field inspection).
      fail(410, "unknown or expired session: the tool call this result answers was not issued by this bridge (likely a restart or idle eviction); the continuation cannot be resumed");
      return;
    }
    // Refresh the advertised tool set + client env from the continuation body too (the Go executor sends
    // `tools`/`clientEnv` on every turn): a C1 fresh-send or C2 re-seed driven from this continuation must
    // advertise the current tools, and a re-seed needs the current env. Harmless when unchanged.
    if (Array.isArray(body.tools)) {
      session.advertise = dedupeByName(body.tools.map((t) => ({ name: t.name, toolName: t.name, providerIdentifier: "cc", description: t.description || "", inputSchema: t.inputSchema || t.parameters || undefined })));
    }
    if (body.clientEnv && typeof body.clientEnv === "object") session.clientEnv = body.clientEnv;
    // Comment 3: tool_results ingestion is NEVER 409'd. Resolving pending tool calls is just promise
    // resolution — safe regardless of any open response. Only the model-output STREAM is single-owner: if a
    // continuation response is already streaming this session's run (concurrent/incremental tool_results),
    // resolve the provided ids into the live run and return a short successful ack on THIS response, leaving
    // the model output on the existing activeRes. Otherwise (the normal case) drive the continuation here.
    if (session.activeRes) {
      res.writeHead(200, SSE_HEADERS);
      // C04 + Comment 6: the concurrent fast path now uses the SAME strict matcher as runTurn (matchToolResults)
      // instead of a bespoke clean-ack-everything loop. The model output stream stays on the existing
      // activeRes; THIS response only ACKS — so an unknown/foreign id (or a nonempty batch that matched nothing
      // and is not a benign re-ack) must be a TYPED ERROR ack, never a clean end_turn that would let the client
      // stop retrying while the run never received the result (the old C04 false-success bug).
      const { matched, unknown } = session.matchToolResults(input.results);
      const batchLen = (input.results || []).length;
      dbg("handleTurn tool_results CONCURRENT ack", sessionId, "matched=" + matched, "of=" + batchLen, "unknown=" + safeJson(unknown));
      // NOTE on input.userText (C1): on the concurrent path the live run already owns activeRes and is
      // streaming the model's answer; the executor folds any trailing user text into the last tool result
      // (C1 belt-and-suspenders), so it reaches the live run there. Driving a separate fresh send HERE would
      // spawn a colliding/orphaned run — so the concurrent path only acks; the live run surfaces userText.
      try {
        if (unknown.length > 0) {
          // Foreign/never-issued id: a genuine desync. Surface it (do NOT fake success) so the client sees it.
          res.write(`data: ${JSON.stringify({ type: "turn_end", stop_reason: "error", error: `unknown tool_call_id ${unknown[0]}: not issued by this session` })}\n\n`);
        } else {
          // matched>0 -> resolved into the live run; matched===0 with no unknown -> all ids were benign
          // re-acks (already-resolved/duplicate). Either way a clean ack is correct (the run is live).
          res.write(`data: ${JSON.stringify({ type: "turn_end", stop_reason: "end_turn" })}\n\n`);
        }
        res.write("data: [DONE]\n\n"); res.end();
      } catch { /* socket closed */ }
      return;
    }
    dbg("handleTurn tool_results -> existing session", sessionId, "pending=" + session.pending.size, "runActive=" + !!session.run);
    res.writeHead(200, SSE_HEADERS);
    return runTurn(req, res, session, model, input, constraints);
  }

  // New-user turn: get-or-create the session, refresh advertised tools/env, then enqueue on the per-session
  // FIFO. The chain serializes concurrent new-user turns (idle -> runs immediately; busy -> waits, kept
  // alive) instead of 409-rejecting them, so no client ever sees a retryable error from a collision.
  let session = sessions.get(sessionId);
  if (!session) { session = new Session(sessionId, cursorKey); sessions.set(sessionId, session); enforceSessionCap(); dbg("handleTurn NEW session", sessionId); }
  else dbg("handleTurn REUSE session", sessionId, "runActive=" + !!session.run, "activeRes=" + !!session.activeRes, "waiters=" + session.waiters);
  if (Array.isArray(body.tools)) {
    session.advertise = dedupeByName(body.tools.map((t) => ({ name: t.name, toolName: t.name, providerIdentifier: "cc", description: t.description || "", inputSchema: t.inputSchema || t.parameters || undefined })));
  }
  if (body.clientEnv && typeof body.clientEnv === "object") session.clientEnv = body.clientEnv;
  if (session.waiters >= MAX_QUEUE_DEPTH) { fail(429, "too many concurrent turns queued for this session"); return; }
  return enqueueTurn(req, res, session, model, input, constraints);
}

// enqueueTurn serializes a new-user turn on the session's FIFO chain. It opens the SSE + a client-facing
// keepalive IMMEDIATELY (so a queued turn looks like one slow-but-live stream, never a silent/failed one),
// waits EVENT-DRIVEN for the prior turn's run to truly complete (session.tail + whenLogicalDone — no
// wall-clock timer), then runs in-order on the same session. A queued waiter's disconnect removes ONLY that
// waiter; it never tears down the session (which would kill the active turn + other waiters).
function enqueueTurn(req, res, session, model, input, constraints) {
  session.waiters++;
  session.touch();
  res.writeHead(200, SSE_HEADERS);
  const ka = setInterval(() => { try { res.write(`data: ${JSON.stringify({ type: "ping" })}\n\n`); } catch {} }, SSE_KEEPALIVE_MS);
  if (ka.unref) ka.unref();
  let canceled = false;
  // BR3: a queued waiter that disconnects must free its slot + session IMMEDIATELY, not after the 10-min
  // abandonment watchdog. So onWaitClose does its own idempotent teardown synchronously (decrement waiters,
  // clear the keepalive, detach itself, end the socket) instead of only flipping `canceled` and waiting for
  // the run ahead to complete before the promotion path tears it down.
  let reaped = false;
  const onWaitClose = () => {
    if (reaped) return;
    reaped = true; canceled = true;
    clearInterval(ka);
    res.off("close", onWaitClose);
    session.waiters = Math.max(0, session.waiters - 1);
    try { res.end(); } catch {}
  };
  res.on("close", onWaitClose);

  const prev = session.tail;
  let releaseNext;
  session.tail = new Promise((r) => { releaseNext = r; });

  prev.then(async () => {
    // The waiter already self-reaped on disconnect (BR3): its teardown ran synchronously, so do NOT
    // double-decrement waiters / double-off here — just release the FIFO so the chain advances.
    if (reaped) return;
    // Atomic handoff: no await between off(onWaitClose) and runTurn (which synchronously registers its own
    // active-turn close handler), so a disconnect can never slip through unhandled at the promotion boundary.
    clearInterval(ka);
    res.off("close", onWaitClose);
    session.waiters = Math.max(0, session.waiters - 1);
    if (canceled) {
      try { res.write(`data: ${JSON.stringify({ type: "turn_end", stop_reason: "end_turn" })}\n\n`); res.write("data: [DONE]\n\n"); res.end(); } catch {}
      return;
    }
    try {
      await runTurn(req, res, session, model, input, constraints); // returns at a tool-pause OR at completion
      await session.whenLogicalDone();                              // hold the FIFO slot until the run TRULY completes
    } catch (e) { dbg("enqueueTurn run error", "session=" + session.id, (e && e.message) || String(e)); }
  }).finally(() => { releaseNext(); });
}

function dedupeByName(tools) {
  const seen = new Set(); const out = [];
  for (const t of tools) { const k = t.toolName || t.name; if (k && !seen.has(k)) { seen.add(k); out.push(t); } }
  return out;
}

async function runTurn(req, res, session, model, input, constraints = {}) {
  session.activeRes = res; session.touch(); session.turnToken++;
  // H08: keep the dispatch seam's native-tool gating current for THIS turn even on a continuation that does
  // not call driveUserSend (the model may emit new native tool calls while resuming). driveUserSend sets it
  // again on a send; this top-level set covers the resume-only path too.
  session.toolChoice = (constraints && constraints.toolChoice) || "";
  res.write(`data: ${JSON.stringify({ type: "session", sessionId: session.id })}\n\n`);

  // Typed keepalive (NOT a ": keepalive" comment — the Go executor forwards only "data: " lines, so a
  // comment never reaches the client). The executor renders {"type":"ping"} into the inbound schema's
  // keepalive frame, resetting the client's idle watchdog during long/quiet turns.
  const keepalive = setInterval(() => { try { res.write(`data: ${JSON.stringify({ type: "ping" })}\n\n`); } catch {} }, SSE_KEEPALIVE_MS);
  if (keepalive.unref) keepalive.unref();
  let settled = false;
  let resolveTurn;
  const turnSettled = new Promise((resolve) => { resolveTurn = resolve; });
  const settleOnce = () => { if (!settled) { settled = true; resolveTurn(); } };
  session.settleTurn = settleOnce;
  // If the client/proxy disconnects MID-turn, settle this turn (so the finally runs and keepalive clears)
  // and cancel the live run. cancel() fires notifyLogicalDone(), advancing the FIFO to the next waiter. A
  // close that arrives AFTER the turn already settled is a normal end-of-turn socket close and must NOT
  // cancel the paused run the next tool_results turn needs. Only DELETE the session when no waiters remain —
  // otherwise a queued turn on the same conversation would be stranded by the active turn's disconnect.
  const onClose = () => {
    if (settled) return;
    settleOnce();
    void session.cancel();
    if (!session.hasQueuedWaiters()) sessions.delete(session.id);
  };
  res.on("close", onClose);

  // driveUserSend performs a model-visible user send on the EXISTING no-timeout agent: it seeds (prepends
  // system + prior history on the FIRST send for the session), applies the enforced constraint instructions,
  // gates the advertised tools by tool_choice, attaches any images, and wires the run's completion callback
  // bound to this run's epoch (mirrors the new-user seed path). It is the single send path shared by the
  // new-user turn, the C1 mixed-turn fresh-send, and the C2/C3 re-seed — so they never drift. extraImages
  // (BR9) are merged in addition to input.images (e.g. images carried inside tool results).
  const driveUserSend = async (userText, extraImages) => {
    session.streamedText = "";   // reset per user turn (NOT across tool-result continuations within a run)
    session.done = false;
    session.lastRunError = null;  // BR2: a fresh run starts clean; a prior run's error must not leak forward
    const agent = await ensureAgent(session, model);
    // ensureAgent's resume/create is a network round-trip; if the client disconnected during it, onClose
    // already settled+cancelled this turn. Bail BEFORE agent.send so we don't spawn an orphan run.
    if (settled) return;
    let text = userText || "";
    // H11: snapshot the seed flags so we can ROLL BACK if agent.send rejects — otherwise a failed first send
    // would leave seeded=true and the retry (reusing this in-memory session) would omit the system + history
    // prelude, answering with missing context. They are committed only after agent.send resolves.
    const seededBefore = session.seeded;
    const seededSystemBefore = session.seededSystem;
    if (!session.seeded) {
      const parts = [];
      if (input.system) parts.push(input.system);
      if (input.history) parts.push("Previous conversation:\n" + input.history);
      if (text) parts.push(text);
      text = parts.join("\n\n");
      session.seeded = true;
      session.seededSystem = input.system || "";   // C3/BR6: remember the seeded system for swap detection
    } else if (input.system && input.system !== session.seededSystem) {
      // C3/BR6: a continuation carried a NEW system prompt (e.g. ExitPlanMode) on an already-seeded session.
      // Prepend it as an updated instruction block so the swap reaches the model, and remember it.
      text = "Updated system instructions:\n" + input.system + (text ? "\n\n" + text : "");
      session.seededSystem = input.system;
    }
    // Enforced per-turn constraints (response_format / stop / token limit / tool_choice) as instructions.
    // H08: record the resolved tool_choice on the session so the dispatch seam can best-effort gate native
    // tools for THIS turn. H09: if a forced specific:<name> tool is not advertised, tell the model it is
    // unavailable (and effectiveAdvertise advertises nothing) instead of widening to other tools.
    const turnToolChoice = (constraints && constraints.toolChoice) || "";
    session.toolChoice = turnToolChoice;
    const forcedUnavailable = forcedToolUnavailable(session.advertise, turnToolChoice);
    const ci = constraintInstructions({ ...constraints, forcedUnavailable });
    if (ci) text = text ? text + "\n\n" + ci : ci;
    // Merge images from the input with any extra images (BR9: tool-result images folded into the send).
    const allImages = [...(Array.isArray(input.images) ? input.images : []), ...(Array.isArray(extraImages) ? extraImages : [])];
    // Build the message first (toSdkImages may throw on a malformed image) BEFORE gating advertisement,
    // so a bad image never leaves session.advertise in the restricted state.
    const msg = allImages.length ? { text, images: toSdkImages(allImages) } : text;
    // tool_choice gates what tools the model SEES this turn (none -> none; specific -> just that one; H09: a
    // missed forced tool -> none, never widened). Restore the full advertised set right after send: the
    // run-request advertisement is built during send, and reconcileToolName still resolves any tool the model
    // calls against the full set.
    const savedAdvertise = session.advertise;
    session.advertise = effectiveAdvertise(session.advertise, turnToolChoice);
    dbg("runTurn -> agent.send", "session=" + session.id, "seeded(after)=" + session.seeded,
      "msgTextLen=" + (typeof msg === "string" ? msg.length : (msg.text || "").length),
      "images=" + allImages.length, "effAdvertise=" + session.advertise.length, "model=" + model);
    const ep = ++session.runEpoch; // this run's epoch; its completion callback must ignore a result if cancel() (or a later run) advanced it
    await als.run({ session }, async () => {
      try {
        session.run = await agent.send(msg, streamCallbacks(session));
      } catch (sendErr) {
        // H11: the send failed — roll the seed flags back to their pre-send values so a retry on this same
        // in-memory session re-prepends the system + history prelude (the first send never actually landed).
        session.seeded = seededBefore;
        session.seededSystem = seededSystemBefore;
        dbg("runTurn agent.send THREW (rolled back seeded)", "session=" + session.id, "seeded->" + session.seeded, (sendErr && sendErr.stack) || (sendErr && sendErr.message) || String(sendErr));
        throw sendErr;
      } finally {
        session.advertise = savedAdvertise;
      }
      // If cancel() ran DURING agent.send (client disconnected mid-send) or a newer run superseded this
      // turn, agent.send still resolved and re-assigned an orphan to session.run. Leaving it there parks
      // the FIFO forever (whenLogicalDone never resolves) and blocks eviction (run!=null). Discard it.
      if (ep !== session.runEpoch || session.done) {
        const orphan = session.run; session.run = null;
        try { await (orphan && orphan.cancel && orphan.cancel()); } catch {}
        session.notifyLogicalDone(); // release the FIFO so the queued waiter advances
        return;
      }
      // Bind completion to THIS run's epoch, not the session: a cancelled/superseded run's late settlement
      // must not tear down a freshly-promoted queued turn that now owns session.run/activeRes/pending.
      session.run.wait()
        .then((r) => { if (ep === session.runEpoch) session.onRunComplete(r); })
        .catch((e) => { if (ep === session.runEpoch) session.onRunError(e); });
    });
  };

  // cancelStaleRun cancels a superseded run WITHOUT settling THIS turn. cancel() calls settle(), which fires
  // session.settleTurn; that handle points to this turn's settleOnce, so a naive cancel here would settle the
  // very turn we are mid-driving (driveUserSend would early-return on `settled`). Detach + restore the handle.
  const cancelStaleRun = async () => {
    session.settleTurn = null;
    await session.cancel();
    session.settleTurn = settleOnce;
    session.done = false; // cancel() set done=true; clear it so a subsequent driveUserSend wires completion
  };

  try {
    dbg("runTurn START", "session=" + session.id, "inputType=" + input.type, "turnToken=" + session.turnToken);
    // C2/BR-C2: a changed history fingerprint on an ESTABLISHED session (e.g. /compact rewrote the
    // non-system history) means the prior run no longer matches the client's view. Re-seed from the replaced
    // history BEFORE matching/continuing, so we resume the right context instead of silently continuing the
    // old conversation. Back-compat: absent fingerprint => no check.
    //
    // SAFETY GATE (`session.run === null`): we only re-seed when NO run is live/paused. A live paused run is
    // exactly what a tool_results continuation is answering; cancelling it on a fingerprint change would
    // silently discard the client's in-flight tool work (the worst kind of lost-work). A genuine /compact
    // arrives as a NEW-USER turn after the prior run completed (run===null), so this gate lets compaction
    // re-seed while never tearing down an answer-in-progress.
    //
    // CROSS-FILE CONTRACT (H13): the Go executor's composerHistoryFingerprint is now GROWTH-STABLE but
    // REWRITE-SENSITIVE — it hashes a bounded retained head (first N non-system messages' role + short text
    // prefix), so a normal tail append leaves it unchanged and ONLY a /compact that rewrites the retained body
    // flips it. So a changed fingerprint here is a genuine rewrite, not normal growth.
    //
    // H12 (cold-restart compact): a WARM session compares against its in-memory historyFingerprint (below).
    // A COLD session (just restarted; in-memory fp is null) has nothing in memory to compare, so the BR-DS
    // probe would resume the durable agent and skip the re-seed even after a /compact. We instead compare the
    // inbound fingerprint to the DURABLE one we persisted for this agentId: if they differ, the retained
    // history was rewritten across the restart -> re-seed the client's compacted history rather than trusting
    // the stale durable agent. No durable fp (fresh conversation / older bridge) -> fall through (a new
    // conversation seeds normally on first send; a same-fp restart trusts durable via BR-DS).
    let forceReseed = false;
    const inboundFp = (typeof input.historyFingerprint === "string" && input.historyFingerprint) ? input.historyFingerprint : null;
    if (inboundFp && session.run === null) {
      const warmChanged = session.historyFingerprint && session.historyFingerprint !== inboundFp;
      let coldCompact = false;
      if (!session.historyFingerprint && !session.seeded) {
        const durableFp = readDurableFingerprint(session.cursorKey, session.agentId || session.id);
        coldCompact = durableFp && durableFp !== inboundFp;
        if (coldCompact) dbg("runTurn COLD-RESTART fingerprint differs from durable -> re-seed (H12 compact)", "session=" + session.id, "agentId=" + (session.agentId || session.id));
      }
      if (warmChanged || coldCompact) {
        dbg("runTurn HISTORY FINGERPRINT CHANGED (no live run) -> re-seed", "session=" + session.id, "warm=" + !!warmChanged, "cold=" + !!coldCompact);
        await cancelStaleRun();
        session.seeded = false;
        forceReseed = true;
      }
    }

    if (forceReseed) {
      // Re-seed path (C2): drive a fresh user send from the replaced history + system + the trailing user
      // text (userText on a continuation, text on a new-user turn). The stale run was cancelled above; the
      // provided tool_results (if any) belonged to it and are intentionally not applied. `reseeding` keeps
      // ensureAgent's BR-DS probe from re-marking the session seeded (we WANT to prepend the new history).
      session.reseeding = true;
      try {
        const seedText = input.userText || input.text || "";
        await driveUserSend(seedText, collectToolResultImages(input));
      } finally {
        session.reseeding = false;
      }
    } else if (input.type === "tool_results") {
      // C-TOOLRESULT-MATCH: match the batch with the ONE shared strict matcher (resolve-by-id -> benign-ack if
      // ever-emitted -> else unknown). C03: the old inline `pending.size===1` fallback is GONE (it lived here);
      // matchToolResults keeps only an explicit `tr.idless`-marked lone-pending escape. Comment 2 idempotency
      // (re-sent already-resolved ids are benign, not fatal) is preserved by the everEmitted/delivered checks.
      const { matched, unknown } = session.matchToolResults(input.results);
      dbg("runTurn tool_results", "session=" + session.id, "matched=" + matched, "of=" + ((input.results || []).length),
        "pending=" + session.pending.size, "undelivered=" + session.undelivered.length, "unknown=" + safeJson(unknown));
      // C1/BR5: a real trailing user message in this mixed turn (set by the executor only for a genuine user
      // message, never a pure system-reminder). If present AND nothing is left to resume (no pending after
      // resolve, or nothing matched at all), drive a fresh user send so the user's message is answered —
      // instead of an empty end_turn. tool-result images (BR9/EX3) are folded into that send. M28: an
      // image-only trailing message (empty userText but images present) is ALSO user payload and must drive a
      // fresh send, so the model answers about the image instead of an empty turn.
      const hasUserText = typeof input.userText === "string" && input.userText.length > 0;
      const hasUserImages = (Array.isArray(input.images) && input.images.length > 0) || collectToolResultImages(input).length > 0;
      const hasUserPayload = hasUserText || hasUserImages;
      // The run will resume and stream the model's answer ONLY when this continuation answered tool(s) and
      // nothing is left pending and the run is still live. In THAT case the trailing user message rode along
      // folded into the last tool result (executor C1 belt-and-suspenders), so a separate fresh send would be
      // redundant AND would cancel the resuming run — so we must NOT drive C1 there.
      const runWillResume = matched > 0 && session.pending.size === 0 && session.run !== null;
      const noneToResume = matched === 0 || session.pending.size === 0;
      // DECISION ORDER (C-TOOLRESULT-MATCH; H06 puts unknown BEFORE the partial-pending clean ack):
      //   1. C1 fresh-send (user payload + nothing will resume)
      //   2. unknown.length>0 -> ERROR turn (a foreign id must never hide behind a partial clean ack)
      //   3. flushUndelivered (late tools delivered this turn)
      //   4. pending.size>0 -> benign empty end_turn (true incremental answer; run stays paused)
      //   5. matched===0 && a paused run DIED upstream -> ERROR turn (BR2; never fake success)
      //   6. else -> benign end_turn (idempotent stale/duplicate ack)
      if (hasUserPayload && noneToResume && !runWillResume) {
        // The user's trailing message/image would otherwise produce an empty end_turn (no output is coming).
        // Answer it with a fresh send. If a run is still live (e.g. matched===0 but unrelated tools are
        // pending), the user is redirecting — cancel the stale run first so we don't spawn a concurrent /
        // orphaned run, then send. cancel() nulls the agent + bumps epoch; driveUserSend re-resumes a live agent.
        dbg("runTurn tool_results -> C1 fresh user send", "session=" + session.id, "matched=" + matched, "pending=" + session.pending.size, "runLive=" + (session.run !== null), "text=" + hasUserText, "images=" + hasUserImages);
        if (session.run !== null) { await cancelStaleRun(); session.seeded = true; }
        // M28: send "" when there is no text but images are present (driveUserSend folds the images in).
        await driveUserSend(hasUserText ? input.userText : "", collectToolResultImages(input));
      } else if (unknown.length > 0) {
        // H06/BR1: a result for an id this session NEVER issued is a genuine desync (e.g. a wrong/foreign id).
        // Surface it as a real error turn BEFORE any partial-pending clean ack, so it is NOT consumed as a
        // clean empty success that silently discards the client's tool work and swallows the bogus id.
        session.sse({ type: "turn_end", stop_reason: "error", error: `unknown tool_call_id ${unknown[0]}: not issued by this session` });
        session.settle();
      } else if (session.flushUndelivered()) {
        // Tools the SDK emitted after the prior turn closed (mid-burst) are now delivered as THIS turn's
        // tool_use batch (Comment 1) so the client can answer them; the run stays paused awaiting them.
      } else if (session.pending.size > 0) {
        // Some delivered tools remain unanswered (true incremental answer). The run is still blocked, so it
        // will neither stream nor complete this turn. Don't error and don't hang: settle a benign empty turn;
        // the run stays paused (bounded by PENDING_TIMEOUT_MS) and the client may answer the rest next.
        session.sse({ type: "turn_end", stop_reason: "end_turn" });
        session.settle();
      } else if (matched === 0) {
        // BR2: nothing matched and nothing pending. If a paused run has since DIED upstream (parallel-tool
        // error etc.), surface that real error instead of a clean empty turn that would fake success.
        if (session.lastRunError && session.run === null) {
          const err = session.lastRunError; session.lastRunError = null;
          dbg("runTurn tool_results matched=0 but run died -> error turn", "session=" + session.id);
          session.sse({ type: "turn_end", stop_reason: "error", error: err });
          session.settle();
        } else {
          // Nothing matched and nothing pending: a stale/duplicate ack (e.g. a client retry of an already-
          // resolved id). Acknowledge cleanly rather than erroring (Comment 2 idempotency) — this is what
          // breaks the old retry storm. When matched>0 and pending==0, the run resumes and streams below.
          session.sse({ type: "turn_end", stop_reason: "end_turn" });
          session.settle();
        }
      }
    } else if (session.run) {
      // Re-entrancy guard: a new user turn while a run is still in flight (paused awaiting tools)
      // would spawn a second concurrent SDK run and orphan the first. CLIProxy should serialize
      // turns per sessionId; reject here as a backstop.
      dbg("runTurn RE-ENTRANT new user turn while run in flight -> reject", "session=" + session.id);
      session.sse({ type: "turn_end", stop_reason: "error", error: "a turn is already in progress for this session" });
      settleOnce();
    } else {
      await driveUserSend(input.text || "", null);
    }
    // C2/BR-C2: record the fingerprint of the history we just seeded/continued, so a LATER changed
    // fingerprint (a future /compact) is detected. Always update on a successful seed/continue. H12: ALSO
    // persist it durably keyed by agentId so a COLD restart can detect a compact that happened while the
    // bridge was down (the in-memory fp is lost on restart; the durable one survives).
    if (inboundFp) {
      session.historyFingerprint = inboundFp;
      writeDurableFingerprint(session.cursorKey, session.agentId || session.id, inboundFp);
    }
    await turnSettled;
  } catch (e) {
    dbg("runTurn CATCH exception", "session=" + session.id, (e && e.stack) || (e && e.message) || String(e));
    if (!settled) session.sse({ type: "turn_end", stop_reason: "error", error: (e && e.message) || String(e) });
  } finally {
    clearInterval(keepalive);
    res.off("close", onClose);
    if (session.activeRes === res) session.activeRes = null;
    try { res.write("data: [DONE]\n\n"); res.end(); } catch {}
  }
}

const server = createServer(async (req, res) => {
  if (req.method === "OPTIONS") { res.setHeader("Access-Control-Allow-Origin", "*"); res.writeHead(204); res.end(); return; }
  if (req.method === "GET" && req.url === "/health") { res.setHeader("Access-Control-Allow-Origin", "*"); res.writeHead(200, { "Content-Type": "application/json" }); res.end(JSON.stringify({ ok: true, patched: true, sessions: sessions.size })); return; }
  // MCP shim endpoint: /mcp/<sessionId>[/<serverKey>]. Dialed by the in-process SDK runtime over loopback
  // (NOT by an external client), so it is authorized by the in-path sessionId + the session lookup, not the
  // inbound X-Bridge-Auth gate /agent/turn uses. Strip the query string, then split the path segments.
  if (req.url && (req.url === "/mcp" || req.url.startsWith("/mcp/"))) {
    const segs = req.url.split("?")[0].split("/").filter(Boolean); // ["mcp", sessionId?, serverKey?]
    const sessionId = segs[1] ? decodeURIComponent(segs[1]) : "";
    const serverKey = segs[2] ? decodeURIComponent(segs[2]) : "";
    if (!sessionId) { res.setHeader("Access-Control-Allow-Origin", "*"); res.writeHead(404, { "Content-Type": "application/json" }); res.end(JSON.stringify({ error: "missing sessionId in /mcp path" })); return; }
    await handleMcp(req, res, sessionId, serverKey); return;
  }
  if (req.method === "POST" && req.url === "/agent/turn") {
    const cursorKey = authorizeRequest(req);
    if (!cursorKey) {
      dbg("POST /agent/turn -> 401 UNAUTHORIZED (authorizeRequest returned empty)");
      // Help diagnose split-brain token config: the SAME token must be set on both the bridge and CLIProxy.
      if (MULTI_TENANT && !req.headers["x-bridge-auth"]) {
        console.warn("[bridge] 401: multi-tenant mode requires X-Bridge-Auth — set the SAME CURSOR_AGENT_BRIDGE_TOKEN on BOTH the bridge and CLIProxy (per-key composer-client-tools-bridge-token or env)");
      }
      res.writeHead(401); res.end("{}"); return;
    }
    let raw;
    try { raw = await readBodyBounded(req); } // M26: bounded read -> 413 past MAX_AGENT_TURN_BYTES
    catch (e) {
      if (e instanceof PayloadTooLargeError) { dbg("POST /agent/turn -> 413 body too large", e.message); res.writeHead(413, { "Content-Type": "application/json" }); res.end(JSON.stringify({ error: e.message })); return; }
      dbg("POST /agent/turn -> 400 body read error", (e && e.message) || String(e)); res.writeHead(400); res.end("{}"); return;
    }
    let body; try { body = JSON.parse(raw); } catch (e) { dbg("POST /agent/turn -> 400 JSON parse error", (e && e.message) || String(e)); res.writeHead(400); res.end("{}"); return; }
    await handleTurn(req, res, body, cursorKey); return;
  }
  res.writeHead(404); res.end(JSON.stringify({ error: "not found" }));
});

// ---- idle session eviction (bounded sessions Map; no leaked agents) ----
const evictTimer = setInterval(() => {
  const cut = nowMs() - SESSION_TTL_MS;
  for (const [id, s] of sessions) { if (!s.activeRes && !s.run && !s.hasQueuedWaiters() && s.lastActivity < cut) { sessions.delete(id); void s.cancel(); } }
  // Multi-tenant only: dispose idle per-user platforms. Single-tenant keeps its single platform resident
  // (it is the common, hot path) — it is never evicted, matching the pre-pool behavior exactly.
  if (MULTI_TENANT) {
    const pcut = nowMs() - PLATFORM_TTL_MS;
    for (const [h, entry] of platforms) {
      if (entry.lastUsed < pcut && !platformHasSession(h)) {
        platforms.delete(h); void disposePlatform(entry);
      }
    }
  }
}, 60000);
if (evictTimer.unref) evictTimer.unref();

// ---- graceful shutdown: stop accepting, settle/cancel sessions, close stores ----
let shuttingDown = false;
async function shutdown() {
  if (shuttingDown) return; shuttingDown = true;
  try { server.close(); } catch {}
  for (const [, s] of sessions) { try { await s.cancel(); } catch {} }
  sessions.clear();
  for (const [, entry] of platforms) { try { await disposePlatform(entry); } catch {} }
  platforms.clear();
  process.exit(0);
}
process.on("SIGTERM", shutdown);
process.on("SIGINT", shutdown);

// Startup self-test (part 1, direct-global): client execution is guaranteed by the dispatch-seam patch
// (__CC_EXEC_U/S route every tool to CC, and native exec is fail-closed behind __CC_ALLOW_NATIVE) — NOT
// by local:{cwd}. local:{cwd:EMPTY_CWD} only pins the SDK's local executor working dir to an empty
// sentinel so getExecutor doesn't default it to the sidecar's own process.cwd(); it is not a cloud/local
// switch. This test PROVES native local execution is unreachable: it feeds the bridge's own __CC_EXEC_U/S
// an exec whose native path throws a sentinel; a routed/rejected result means we returned before touching
// native exec. Covers the representative native FS/exec tools (read/write/shell) plus an exotic case
// (computerUse); each must be routed or rejected.
async function selfTestNativeUnreachable() {
  const tripwire = { exec: { execute() { throw new Error("__CC_NATIVE_REACHED__"); } } };
  for (const cas of ["readArgs", "writeArgs", "shellArgs", "computerUseArgs"]) {
    const t = { message: { case: cas }, execId: "selftest" };
    try {
      await globalThis.__CC_EXEC_U(tripwire, {}, {}, t);
      // requestContext/prechecks resolve synthetically; FS/exotic cases must REJECT (no ALS session here).
      throw new Error(`self-test: ${cas} resolved natively/unexpectedly (fail-closed broken)`);
    } catch (e) {
      if (/__CC_NATIVE_REACHED__/.test(e && e.message)) {
        throw new Error(`self-test: native local execution is REACHABLE for ${cas} — refusing to start`);
      }
      // expected: rejected "not supported"/"outside a session" — native never touched.
    }
  }
  // The streaming dispatcher must also never reach native for a streaming exec case.
  try {
    const gen = globalThis.__CC_EXEC_S(tripwire, {}, {}, { message: { case: "shellStreamArgs" }, execId: "selftest" });
    await gen.next();
    throw new Error("self-test: shellStreamArgs resolved natively/unexpectedly (fail-closed broken)");
  } catch (e) {
    if (/__CC_NATIVE_REACHED__/.test(e && e.message)) {
      throw new Error("self-test: native local execution is REACHABLE for shellStreamArgs — refusing to start");
    }
  }
}

// Startup self-test (part 2, bundle seam): part 1 tests the bridge's own globals in isolation — it cannot
// catch a patch that "applied" but whose seam fails to intercept the SDK's REAL dispatch at runtime. The
// patcher therefore exposes the EXACT seam expressions it injected at the live dispatch sites as
// __CC_SELFTEST_DISPATCH_U/S. Here we drive those with a tripwire executor whose native exec.execute()
// throws a sentinel. We first run a POSITIVE CONTROL (routing disabled => the seam MUST fall through to
// native, proving the harness genuinely reaches native), then assert that with routing enabled the seam
// NEVER reaches native. Fail startup either way.
async function selfTestBundleSeam() {
  const U = globalThis.__CC_SELFTEST_DISPATCH_U;
  const S = globalThis.__CC_SELFTEST_DISPATCH_S;
  if (typeof U !== "function" || typeof S !== "function") {
    throw new Error("self-test: patched SDK bundle did not install the dispatch-seam harness (__CC_SELFTEST_DISPATCH_*) — patch missing/stale; refusing to start");
  }
  const tripwire = { exec: { execute() { throw new Error("__CC_NATIVE_REACHED__"); } } };
  const readMsg = { message: { case: "readArgs" }, execId: "selftest-seam" };
  const streamMsg = { message: { case: "shellStreamArgs" }, execId: "selftest-seam" };
  const reachedNative = async (fn, arg) => {
    try { const r = fn(tripwire, {}, {}, arg); if (r && typeof r.next === "function") await r.next(); else await r; }
    catch (e) { return /__CC_NATIVE_REACHED__/.test(e && e.message); }
    return false;
  };

  // Positive control (BOTH seams): disable routing so each seam's native branch is taken; each harness MUST
  // reach native, otherwise the assertion below would be vacuous (a harness that never calls native can't
  // detect a miss). NOTE: this mutates shared globals, so the two startup self-tests run SEQUENTIALLY (never
  // concurrently) — selfTestNativeUnreachable reads these same globals and must not observe the disabled window.
  const savedU = globalThis.__CC_EXEC_U, savedS = globalThis.__CC_EXEC_S, savedAllow = globalThis.__CC_ALLOW_NATIVE;
  globalThis.__CC_EXEC_U = undefined; globalThis.__CC_EXEC_S = undefined; globalThis.__CC_ALLOW_NATIVE = true;
  let controlU, controlS;
  try {
    controlU = await reachedNative(U, readMsg);
    controlS = await reachedNative(S, streamMsg);
  } finally {
    globalThis.__CC_EXEC_U = savedU; globalThis.__CC_EXEC_S = savedS; globalThis.__CC_ALLOW_NATIVE = savedAllow;
  }
  if (!controlU || !controlS) {
    throw new Error(`self-test: seam harness did not reach native under the positive control (unary=${controlU}, stream=${controlS}) — not exercising the real dispatch path; refusing to start`);
  }

  // Real check: with routing enabled (the live bridge state) the seam must route to the bridge globals and
  // NEVER touch native, for both the unary and streaming dispatch sites.
  if (await reachedNative(U, readMsg)) {
    throw new Error("self-test: patched unary seam reached NATIVE exec — fail-closed broken; refusing to start");
  }
  if (await reachedNative(S, streamMsg)) {
    throw new Error("self-test: patched stream seam reached NATIVE exec — fail-closed broken; refusing to start");
  }
}

if (RUN_AS_MAIN) {
  // Single-tenant needs CURSOR_API_KEY; multi-tenant needs CURSOR_AGENT_BRIDGE_TOKEN (per-user keys arrive
  // on each request). Require at least one so the bridge always has a way to obtain a Cursor credential.
  if (!API_KEY && !BRIDGE_TOKEN) { console.error("[bridge] set CURSOR_API_KEY (single-tenant) or CURSOR_AGENT_BRIDGE_TOKEN (multi-tenant) — refusing to start"); process.exit(1); }
  // mkdir the store + empty cwd so the SDK's executor-init / git-root probe doesn't ENOENT, and
  // refuse to start if STATE_ROOT is not writable (the SDK persists session/checkpoint state there).
  try { mkdirSync(EMPTY_CWD, { recursive: true }); accessSync(STATE_ROOT, constants.W_OK); }
  catch (e) { console.error(`[bridge] STATE_ROOT ${path.resolve(STATE_ROOT)} is not writable: ${e.message}`); process.exit(1); }
  console.log(`[bridge] mode=${MULTI_TENANT ? "multi-tenant (per-key platforms, X-Bridge-Auth gated)" : "single-tenant (one CURSOR_API_KEY)"} durable stateRoot=${path.resolve(STATE_ROOT)} (sqlite session+checkpoint state is written here; NOT a 'zero-FS' guarantee — only TOOL EXECUTION is FS-isolated to the client)`);
  // fail-closed: confirm the routing globals are installed before listening.
  if (typeof globalThis.__CC_EXEC_U !== "function" || typeof globalThis.__CC_EXEC_S !== "function" || typeof globalThis.__CC_GET_ADVERTISE__ !== "function") {
    console.error("[bridge] routing globals not installed — refusing to start"); process.exit(1);
  }
  // fail-closed: load + assert the patched SDK now (loadSdk is lazy elsewhere so unit tests can import
  // this module without the SDK's native deps); refuse to start if it is missing or unpatched.
  try { loadSdk(); } catch (e) { console.error("[bridge]", (e && e.message) || e); process.exit(1); }
  // SEQUENTIAL, not Promise.all: selfTestBundleSeam temporarily nulls globalThis.__CC_EXEC_U/S for its
  // positive control, and selfTestNativeUnreachable reads those same globals — running them concurrently
  // would let the second neuter the first (it would catch a manufactured TypeError instead of the real
  // routing result). Sequencing removes the shared-global window entirely.
  selfTestNativeUnreachable()
    .then(() => selfTestBundleSeam())
    .then(() => server.listen(PORT, "127.0.0.1", () => console.log(`[cursor-agent-bridge] listening on http://127.0.0.1:${PORT} (patched CJS, fail-closed, native-unreachable + bundle-seam self-tests passed, durable stateRoot=${STATE_ROOT})`)))
    .catch((e) => { console.error("[bridge]", (e && e.message) || e); process.exit(1); });
}

export { CC_CASES, headlessRequestContext, Session, reconcileExport, toSdkImages, constraintInstructions, effectiveAdvertise, forcedToolUnavailable, nativeToolBlockedByChoice, blockedNativeResult, typedUnavailableResult, TYPED_UNAVAILABLE_U, parseShellContent, ccToolId, authorizeRequest, authorizeRequestWith, platformHasSession, keyHash, loadSdk, selfTestNativeUnreachable, selfTestBundleSeam, handleTurn, sessions, platforms, collectToolResultImages, isConversationTooLong, ensureAgent, buildMcpServers, mcpServerKeyForTool, mcpToolsForServer, mcpDispatch, handleMcp, MCP_GROUPING, MCP_SHIM_ENABLED, readBodyBounded, PayloadTooLargeError, MAX_AGENT_TURN_BYTES };
function reconcileExport(advertise, want) { const s = new Session("x"); s.advertise = advertise; return s.reconcileToolName(want); }
