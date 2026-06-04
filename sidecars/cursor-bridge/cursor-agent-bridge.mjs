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

// ADD-64: strict integer env parser. Raw parseInt silently mangles common bad config — parseInt("10m")===10
// (a 10-MINUTE timeout collapses to 10 MILLISECONDS), parseInt("abc")===NaN (Node timers treat NaN as ~0 and
// NaN comparisons silently disable a cap). Every duration/count env below MUST go through this so a typo or
// "10m"-style value degrades to the DOCUMENTED DEFAULT (with a console.warn) instead of an immediate timeout
// or a disabled cap. Only a strictly non-negative integer string is accepted; anything else -> def. Bounds
// are inclusive; an in-bounds default is assumed by callers. This is config validation, NOT a data-path
// timeout (AGENTS.md). Generalizes the existing MAX_AGENT_TURN_BYTES Number.isFinite guard to ALL envs.
function envInt(name, def, { min = 0, max = Number.MAX_SAFE_INTEGER } = {}) {
  const raw = process.env[name];
  if (raw == null || String(raw).trim() === "") return def;
  const s = String(raw).trim();
  if (!/^[0-9]+$/.test(s)) {
    console.warn(`[bridge] ${name}="${raw}" is not a non-negative integer — using default ${def}`);
    return def;
  }
  const n = Number(s);
  if (!Number.isSafeInteger(n) || n < min || n > max) {
    console.warn(`[bridge] ${name}="${raw}" is out of range [${min}, ${max}] — using default ${def}`);
    return def;
  }
  return n;
}

const PORT = envInt("CURSOR_AGENT_BRIDGE_PORT", 9798, { min: 1, max: 65535 });
// ADD-105: the interface the bridge binds. Defaults to loopback (127.0.0.1) — the safe single-host topology
// (CLIProxy dials it over loopback). The Go proxy DOES support a remote CURSOR_AGENT_BRIDGE_URL, so an operator
// running the bridge as a separate container can set CURSOR_AGENT_BRIDGE_HOST=0.0.0.0; binding a non-loopback
// interface exposes /agent/turn (which carries the per-user Cursor bearer) on the network, so it is gated:
// validateBindHost REQUIRES multi-tenant auth (BRIDGE_TOKEN) on a non-loopback bind, refusing to start
// (fail-closed) otherwise unless an explicit insecure opt-in is set, and a startup warning is emitted either way.
const BRIDGE_HOST = (process.env.CURSOR_AGENT_BRIDGE_HOST || "127.0.0.1").trim() || "127.0.0.1";
const ALLOW_INSECURE_BIND = process.env.CURSOR_AGENT_ALLOW_INSECURE_BIND === "1" || process.env.CURSOR_AGENT_ALLOW_INSECURE_BIND === "true";
const API_KEY = process.env.CURSOR_API_KEY || "";
// STATE_ROOT holds the SDK's DURABLE agent/run state (sqlite checkpoint + event stores). It MUST live on a
// PERSISTENT path: on an ephemeral container fs every restart wipes all durable agents, so the next turn of
// every live conversation can't resume its agent and falls back to a full history reseed (the reseed-storm
// incidents). Precedence: explicit CURSOR_AGENT_STATE_ROOT > a subdir of the Railway persistent volume
// (RAILWAY_VOLUME_MOUNT_PATH, set automatically when a volume is attached) > cwd (dev default). A durable run
// is re-attachable across a process restart via platform.getRun(runId) ONLY when this path survives the restart.
const STATE_ROOT = process.env.CURSOR_AGENT_STATE_ROOT
  || (process.env.RAILWAY_VOLUME_MOUNT_PATH
    ? path.join(process.env.RAILWAY_VOLUME_MOUNT_PATH, ".cursor-agent-store")
    : path.join(process.cwd(), ".cursor-agent-store"));
const EMPTY_CWD = path.join(STATE_ROOT, ".empty");
// PENDING_TIMEOUT_MS / SESSION_TTL_MS must be POSITIVE (a 0 watchdog would reap a tool the instant it is
// emitted; a 0 TTL would evict a session immediately) — floor them at 1ms via min:1.
const PENDING_TIMEOUT_MS = envInt("CURSOR_AGENT_PENDING_TIMEOUT_MS", 600000, { min: 1 });
const SESSION_TTL_MS = envInt("CURSOR_AGENT_SESSION_TTL_MS", 1800000, { min: 1 });
const MAX_SESSIONS = envInt("CURSOR_AGENT_MAX_SESSIONS", 1000, { min: 1 });
const SSE_KEEPALIVE_MS = 15000;
// Tool-batch coalescing window. The @cursor/sdk never pauses for tools — it streams tool calls in waves —
// so this debounce merges a wave (emits <window apart) into ONE turn_end. It is a pure latency<->round-trip
// knob, NOT a correctness control: tools emitted after a turn closes are buffered + re-delivered next turn
// regardless (see emitToolUse/flushUndelivered), so any value is safe. Raise it (e.g. 150-200) to coalesce
// slower waves into fewer client round-trips at the cost of a little per-turn latency; lower it for snappier
// turns and more round-trips. Default 60 preserves the original behavior.
// TOOL_BATCH_MS may legitimately be 0 (flush immediately, no coalescing) so min:0 is correct here.
const TOOL_BATCH_MS = envInt("CURSOR_AGENT_TOOL_BATCH_MS", 60, { min: 0 });
// Elegant step-boundary refinement (review): the SDK announces each tool of a step via a `tool-call-started`
// delta BEFORE our dispatch seam emits it. When more have been announced for this step than we have delivered,
// the pause waits one more debounce window for the rest of the batch — capturing a slow tool wave in ONE
// turn_end instead of spilling the tail into the next turn. BOUNDED so an over-count (an announced tool that
// never dispatches) can never hang the turn: after this many extensions we pause anyway (the debounce floor).
// This only ever DELAYS the pause to capture the full batch; it never pauses earlier, so it cannot strand.
const TOOL_BATCH_MAX_EXTENSIONS = envInt("CURSOR_AGENT_TOOL_BATCH_MAX_EXTENSIONS", 8, { min: 0 });
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
// EX3 (clean image path): a tool-result IMAGE is folded into the proto McpToolResult as McpImageContent, so the
// model sees it on RESUME — no fresh-send side-channel, and multi-tool/partial batches need no special handling
// (each tool's image rides its OWN dispatchMcp result). ON by default — VERIFIED end-to-end that Cursor forwards
// McpImageContent to composer-2.5 (composer read a token image returned this way). Set
// CURSOR_COMPOSER_MCP_IMAGE_RESULTS=0 to fall back to the fresh-send fold (kept intact as the escape hatch, and
// always used for url-form images, which McpImageContent's base64 `data` field cannot carry). Read at call time
// (not a load-time const) so tests can exercise both paths.
function mcpImageResultsEnabled() {
  const v = process.env.CURSOR_COMPOSER_MCP_IMAGE_RESULTS;
  return v !== "0" && v !== "false";
}
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
const MAX_QUEUE_DEPTH = envInt("CURSOR_AGENT_MAX_QUEUE_DEPTH", 32, { min: 1 });
// M26: cap how many bytes a single /agent/turn (and /mcp) request body may read into memory, so a buggy or
// hostile authenticated CLIProxy cannot OOM the sidecar (which would kill every session on this bridge). The
// cap is GENEROUS — large histories + base64 images are legitimate — defaulting to 64 MB and env-overridable
// via MAX_AGENT_TURN_BYTES; a real conversation must never hit it. Past the cap we stop concatenating and
// return 413 (a READ bound, not an upstream wall-clock timeout, so it is allowed by AGENTS.md).
// ADD-64: generalized to the shared envInt guard (was a bespoke Number.isFinite check); min:1 so a 0/blank
// can never disable the OOM bound (it falls back to the 64 MB default).
const MAX_AGENT_TURN_BYTES = envInt("MAX_AGENT_TURN_BYTES", 64 * 1024 * 1024, { min: 1 });
// ADD-100: per-session SSE output-queue cap. When a client/proxy applies sustained backpressure (res.write
// returns false and 'drain' never fires) the bridge queues unsent SSE payloads in Session.outQueue; this
// bounds that queue so a slow/stuck client cannot OOM the sidecar via unbounded Node write buffering. Past the
// cap the turn is cancelled with a typed transport error (never a fake success). This is a MEMORY bound, NOT a
// data-path wall-clock timeout (allowed by AGENTS.md); default 8 MB is generous for a quiet/slow client.
const COMPOSER_OUT_QUEUE_MAX_BYTES = envInt("CURSOR_AGENT_OUT_QUEUE_MAX_BYTES", 8 * 1024 * 1024, { min: 1 });
// ADD-95 (bridge backstop): cap live tool-result content the bridge resolves into a pending, distinct from the
// executor's history bound. The EXECUTOR's truncateCursorToolResultLive is authoritative and normally wins;
// this is a slightly HIGHER cap so it only fires for content that bypassed the executor cap (a defense-in-depth
// backstop). Both sides stamp the SAME marker substring 'truncated by proxy' so callers and tests agree.
const COMPOSER_LIVE_TOOL_RESULT_MAX_BYTES = envInt("CURSOR_AGENT_LIVE_TOOL_RESULT_MAX_BYTES", 320 * 1024, { min: 1 });
// ADD-103: cap the serialized response_format / json_schema inlined into the prompt as a best-effort
// instruction. A large generated schema (nested $defs, many enums) would otherwise bloat the prompt, leak more
// than necessary, and risk ERROR_CONVERSATION_TOO_LONG — all for a constraint the composer path can only honor
// best-effort anyway (the unsupportedHardGuarantees advisory still tells the model it is non-enforced). Past
// the cap we inline a short note instead of the full schema. A SIZE bound on prompt text, not a timeout.
const COMPOSER_SCHEMA_INLINE_MAX_BYTES = envInt("CURSOR_AGENT_SCHEMA_INLINE_MAX_BYTES", 8 * 1024, { min: 1 });
// Shared SSE response headers (unbuffered, so keepalives reach the wire end-to-end).
const SSE_HEADERS = { "Content-Type": "text/event-stream", "Cache-Control": "no-cache", Connection: "keep-alive", "X-Accel-Buffering": "no" };
function formatSseData(obj) { return `data: ${JSON.stringify(obj)}\n\n`; }
// Multi-tenant (opt-in): when CURSOR_AGENT_BRIDGE_TOKEN is set, X-Bridge-Auth gates access and the
// Authorization bearer is the PER-USER Cursor key (each gets an isolated SDK platform + stateRoot).
// When unset (default), behavior is single-tenant: the bearer must equal CURSOR_API_KEY and is the key.
const BRIDGE_TOKEN = process.env.CURSOR_AGENT_BRIDGE_TOKEN || "";
const MULTI_TENANT = BRIDGE_TOKEN !== "";
// ADD-52: in multi-tenant mode the Authorization bearer is the PER-USER Cursor key CLIProxy forwards; each
// user must therefore present their own key. The old code fell back to the global CURSOR_API_KEY when the
// bearer was missing (misconfig / proxy stripping / direct sidecar access), collapsing tenant isolation and
// contaminating the default account's durable SDK state. Default: REQUIRE a non-empty per-user bearer. The
// single-user compatibility fallback to CURSOR_API_KEY is gated behind an explicit opt-in flag.
const ALLOW_DEFAULT_KEY = process.env.CURSOR_AGENT_ALLOW_DEFAULT_KEY === "1" || process.env.CURSOR_AGENT_ALLOW_DEFAULT_KEY === "true";
const MAX_PLATFORMS = envInt("CURSOR_AGENT_MAX_PLATFORMS", 64, { min: 1 });
const PLATFORM_TTL_MS = envInt("CURSOR_AGENT_PLATFORM_TTL_MS", 3600000, { min: 1 });
const RUN_AS_MAIN = process.argv[1] === fileURLToPath(import.meta.url);

// ---- load the PATCHED CJS bundle (NOT `import`, which resolves to unpatched dist/esm); assert patched ----
// Loading is lazy (loadSdk) so this module can be imported for unit tests without pulling the SDK's
// heavy/native deps; the real server calls loadSdk() at startup (fail-closed) BEFORE it accepts traffic.
const require = createRequire(import.meta.url);
// assertPatched greps the bundle's first 64 bytes for the PATCH-MARKER 'cursor-composer-clienttools-patched-v1'.
// Comment 7 / ADD-102 / PATCH-MARKER-CAPABILITY contract: the patcher (scripts/apply-clienttools-patch.cjs)
// keeps this marker at -v1 and instead does a CAPABILITY check (the marker alone does not prove a bundle is
// current — a partial/stale patch with the marker but a missing seam fails closed in the patcher with a
// 'stale-bundle' error). So this grep STAYS at -v1 and is intentionally NOT bumped. If the team ever bumps the
// patcher's MARK to -v2, this grep string MUST be bumped in LOCKSTEP (same commit) or an old patched bundle
// would be wrongly accepted here. The full per-seam capability verification lives on the patcher side; the
// runtime self-tests (selfTestBundleSeam / selfTestResultSerialization) additionally prove the live seams work.
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
function authorizeRequestWith(headers, { apiKey, bridgeToken, allowDefaultKey = false }) {
  const h = headers || {};
  const m = /^Bearer\s+(.+)$/i.exec(h.authorization || "");
  const bearer = m ? m[1] : "";
  if (bridgeToken) {
    if (!constEq(h["x-bridge-auth"], bridgeToken)) return "";
    // ADD-52: require a per-user bearer in multi-tenant mode; only fall back to the global key when the
    // operator explicitly opted into single-user compatibility (CURSOR_AGENT_ALLOW_DEFAULT_KEY=1).
    if (bearer) return bearer;
    return allowDefaultKey ? apiKey : "";
  }
  return constEq(bearer, apiKey) ? apiKey : "";
}
function authorizeRequest(req) {
  return authorizeRequestWith((req && req.headers) || {}, { apiKey: API_KEY, bridgeToken: BRIDGE_TOKEN, allowDefaultKey: ALLOW_DEFAULT_KEY });
}

// isConversationTooLong matches the Cursor "conversation too long" error class (BR-PL). When a run dies with
// this, the session is poisoned (every resume re-sends the same over-budget history); the caller drops the
// session so the NEXT turn re-seeds a fresh one. Matches the SDK's ERROR_CONVERSATION_TOO_LONG token plus a
// generic phrasing as a safety net (the exact upstream string may vary across Cursor releases).
function isConversationTooLong(msg) {
  return /ERROR_CONVERSATION_TOO_LONG|conversation (is )?too long/i.test(String(msg || ""));
}

// parseShellContent accepts either a plain stdout string or a JSON OBJECT the Go/CC side may send
// carrying a structured result {stdout, stderr, exitCode, aborted} so non-zero exits are not masked.
// ADD-42: a STRING is ALWAYS treated as raw stdout, even when it happens to begin with "{". The old code
// JSON.parsed a string that started with "{" and, if it carried exitCode/stdout keys, treated it as a
// privileged result envelope — so a command whose REAL stdout was e.g. `{"exitCode":1,"stdout":"x"}` (an
// untrusted project script printing JSON) could forge its own exit code / stdout / stderr to the model. The
// structured channel is the OBJECT branch only (the Go side sends an actual object for structured shell
// results, never a JSON string), which a command's stdout can never collide with. A string -> stdout, full stop.
function parseShellContent(c) {
  if (c && typeof c === "object") {
    return { stdout: String(c.stdout ?? ""), stderr: String(c.stderr ?? ""), exitCode: Number(c.exitCode ?? c.exit_code ?? 0), aborted: Boolean(c.aborted) };
  }
  return { stdout: String(c ?? ""), stderr: "", exitCode: 0, aborted: false };
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

// truncateLiveToolResult is the BRIDGE BACKSTOP for ADD-95: it caps a live tool-result STRING the bridge is
// about to resolve into a pending (and thus into the resuming Cursor run). The EXECUTOR's
// truncateCursorToolResultLive is authoritative and uses a SMALLER cap, so it normally wins and this never
// fires for executor-routed content; this only trims content that bypassed the executor cap. The marker
// substring 'truncated by proxy' is PINNED so both halves and the tests agree. Only STRING content is capped —
// a structured OBJECT (the Go side's shell envelope etc.) is passed through untouched (truncating it would
// corrupt the structured channel). Byte-accurate (not char-length) so multibyte content is bounded correctly.
function truncateLiveToolResult(content, cap = COMPOSER_LIVE_TOOL_RESULT_MAX_BYTES) {
  if (typeof content !== "string") return content;
  const total = Buffer.byteLength(content);
  if (total <= cap) return content;
  // Keep a byte-bounded prefix (slice by chars then verify bytes; a final byte-safe trim avoids splitting a
  // multibyte codepoint mid-sequence).
  let kept = content.slice(0, cap);
  while (Buffer.byteLength(kept) > cap && kept.length) kept = kept.slice(0, -1);
  return kept + `\n[tool result truncated by proxy: kept ${Buffer.byteLength(kept)}/${total} bytes]`;
}

const TOOL_CHOICE_SPECIFIC_PREFIX = "specific:";

function toolChoiceSpecificName(toolChoice) {
  const tc = toolChoice || "";
  if (!tc.startsWith(TOOL_CHOICE_SPECIFIC_PREFIX)) return null;
  return tc.slice(TOOL_CHOICE_SPECIFIC_PREFIX.length);
}

function advertisedToolName(t) {
  return t.toolName || t.name;
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
function constraintInstructions({ toolChoice, responseFormat, stop, maxTokens, forcedUnavailable, unsupportedHardGuarantees } = {}) {
  const lines = [];
  const tc = toolChoice || "";
  const specificNm = toolChoiceSpecificName(tc);
  if (forcedUnavailable && specificNm != null) {
    // H09: never widen to other tools; tell the model the forced tool cannot be used this turn.
    lines.push(`The tool "${specificNm}" was required for this request but is NOT available. Do not call any other tool as a substitute; explain that the requested tool is unavailable.`);
  } else if (tc === "required") {
    lines.push("You MUST call one of the available tools to fulfill this request; do not produce a final answer until you have called at least one tool.");
  } else if (specificNm != null) {
    lines.push(`You MUST call the tool named "${specificNm}" to fulfill this request, and you may call only that tool.`);
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
      // ADD-103: bound the inlined schema. JSON.stringify can throw on a cyclic value; if so, fall back to the
      // size-note branch too (we cannot safely inline it).
      let serialized = null;
      try { serialized = JSON.stringify(schema); } catch { serialized = null; }
      if (serialized != null && Buffer.byteLength(serialized) <= COMPOSER_SCHEMA_INLINE_MAX_BYTES) {
        lines.push("Respond with a single valid JSON value that conforms EXACTLY to this JSON Schema (no prose, no markdown code fences):\n" + serialized);
      } else {
        const n = serialized != null ? Buffer.byteLength(serialized) : "?";
        lines.push(`Respond with a single valid JSON value only — no prose, no markdown code fences. (A response_format/schema too large to inline (${n} bytes) was requested; it is best-effort only and cannot be hard-enforced on this path.)`);
      }
    }
  }
  if (Array.isArray(stop) && stop.length) {
    lines.push("Stop your response immediately before emitting any of these sequences: " + stop.map((s) => JSON.stringify(s)).join(", ") + ".");
  }
  if (Number.isFinite(maxTokens) && maxTokens > 0) {
    lines.push(`Keep your entire response within approximately ${maxTokens} tokens.`);
  }
  // ADD-72 / H20 / H21: the executor flags requested guarantees the composer path cannot hard-enforce
  // server-side (sampling temperature/top_p/n, stop sequences, token cap, response_format, parallel-call
  // limits, built-in tools). Surface them to the model explicitly so a client that asked for an
  // un-enforceable guarantee is told it is best-effort — never silently pretended-enforced.
  if (Array.isArray(unsupportedHardGuarantees) && unsupportedHardGuarantees.length) {
    lines.push("Note: the following requested constraints cannot be hard-enforced on this path and are best-effort only: " + unsupportedHardGuarantees.join("; ") + ".");
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
  const nm = toolChoiceSpecificName(tc);
  if (nm != null) {
    return adv.filter((t) => advertisedToolName(t) === nm); // H09: empty when forced tool not advertised
  }
  return adv;
}

// forcedToolUnavailable reports whether a forced specific:<name> tool_choice cannot be satisfied by the
// advertised set (H09). When true the turn must tell the model the tool is unavailable instead of silently
// offering other tools or pretending the constraint held.
function forcedToolUnavailable(advertise, toolChoice) {
  const nm = toolChoiceSpecificName(toolChoice);
  if (nm == null) return false;
  const adv = Array.isArray(advertise) ? advertise : [];
  return !adv.some((t) => advertisedToolName(t) === nm);
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
  if (toolChoiceSpecificName(tc) != null) return true;
  return false;
}

// toolManifest renders a client-agnostic system preamble that names EVERY tool offered to the model this turn
// (name + its own description), so composer-2.5 treats the client's advertised/MCP tools as first-class and
// actually CALLS the right one for each action instead of doing or simulating the work itself (composer is tuned
// for its NATIVE tools and otherwise under-uses foreign client tools — see the MCP-shim note above). It is built
// DYNAMICALLY from whatever tools were advertised (any client, any toolset, no hardcoding) and from the
// tool_choice-EFFECTIVE set, so it never lists a tool the turn won't accept (none -> "", specific:<x> -> just x).
// Bounded so a 60-tool turn stays a few KB. Returns "" when no tools are offered.
const TOOL_MANIFEST_DESC_MAX = envInt("CURSOR_COMPOSER_TOOL_MANIFEST_DESC_MAX", 0, { min: 0 }); // 0 = names only (the model already has the FULL verbatim descriptions via tools/list)
const TOOL_MANIFEST_MAX_BYTES = envInt("CURSOR_COMPOSER_TOOL_MANIFEST_MAX_BYTES", 16384, { min: 256 });
// CURSOR_COMPOSER_TOOL_MANIFEST selects WHERE the tool manifest is delivered (client-agnostic):
//   rule           -> a system-level always-apply CursorRule via requestContext.rules (per-session, authoritative)
//   prompt         -> appended to the user turn's text (soft, in-band)
//   both (default) -> both channels (prompt is reliably read in-band; rule is authoritative + cache-friendly)
//   off|0|false|none -> neither
const TOOL_MANIFEST_MODE = (() => {
  const v = String(process.env.CURSOR_COMPOSER_TOOL_MANIFEST ?? "").trim().toLowerCase();
  if (v === "0" || v === "false" || v === "off" || v === "none") return "off";
  if (v === "prompt" || v === "text") return "prompt";
  if (v === "rule" || v === "rules") return "rule";
  if (v === "both") return "both";
  return "both"; // default (unset/"1"/"true"): rule-only was under-obeyed in practice; in-band prompt is reliably read
})();
const TOOL_MANIFEST_IN_PROMPT = TOOL_MANIFEST_MODE === "prompt" || TOOL_MANIFEST_MODE === "both";
const TOOL_MANIFEST_IN_RULE = TOOL_MANIFEST_MODE === "rule" || TOOL_MANIFEST_MODE === "both";

// ADD-106 (Comment 3, bridge half): per-LOGICAL-RUN agentic-loop bounds. These are COUNT bounds, not timers,
// so they comply with the repo's no-post-connection-timeout policy (nothing here arms a wall-clock deadline on
// the established data path). A composer run that loops forever — re-pausing for tools without ever producing a
// terminal answer, or hammering the SAME tool with the SAME args — would otherwise stream/pause indefinitely.
//   MAX_TOOL_ROUNDS: how many tool-result rounds (pauseForTools cycles) one logical run may take before we
//     terminate it. The default is GENEROUS (>= 200) so a legitimately long agentic task is NEVER truncated;
//     it only catches a genuine runaway. Counted per logical run; reset when a fresh send starts (driveUserSend).
//   MAX_REPEAT_TOOL: trip when the SAME (tool-name + args-signature) recurs this many times CONSECUTIVELY across
//     rounds (a tight "call read(/x) over and over" loop). 0 disables the repeat detector (rounds bound stays).
// On trip we terminate the run with a MODEL/CLIENT-visible turn_end{stop_reason:"error", error:...} — never a
// clean end_turn/[DONE] (that would falsely report a runaway loop as a successful answer).
const COMPOSER_MAX_TOOL_ROUNDS = envInt("CURSOR_COMPOSER_MAX_TOOL_ROUNDS", 200, { min: 1 });
const COMPOSER_MAX_REPEAT_TOOL = envInt("CURSOR_COMPOSER_MAX_REPEAT_TOOL", 8, { min: 0 });
function toolManifest(advertised) {
  const adv = Array.isArray(advertised) ? advertised : [];
  if (!adv.length) return "";
  // Group tools by their MCP server key — that is how composer-2.5 actually SEES them (model-confirmed: these are
  // NOT top-level tools; each lives on an MCP server and is invoked as an MCP tool call, server + tool — so it
  // read files itself instead of calling Workflow). Naming the server + framing them as MCP tools is what makes
  // it invoke them. Grouping mirrors buildMcpServers/tools/list (mcpServerKeyForTool) so it matches what it sees.
  const byServer = new Map(); // serverKey -> ["- name[: desc]", ...] (insertion order preserved)
  let bytes = 0;
  let truncated = false;
  for (const t of adv) {
    const name = (t && (t.toolName || t.name)) || "";
    if (!name) continue;
    const server = mcpServerKeyForTool(name, MCP_GROUPING) || "claude-code";
    // Names only by default (TOOL_MANIFEST_DESC_MAX=0); the full verbatim descriptions already reach the model via
    // tools/list. Set CURSOR_COMPOSER_TOOL_MANIFEST_DESC_MAX>0 for per-tool descriptions up to that length.
    let desc = "";
    if (TOOL_MANIFEST_DESC_MAX > 0 && t && t.description) {
      desc = String(t.description).replace(/\s+/g, " ").trim();
      if (desc.length > TOOL_MANIFEST_DESC_MAX) desc = desc.slice(0, TOOL_MANIFEST_DESC_MAX - 1) + "…";
    }
    const line = desc ? `- ${name}: ${desc}` : `- ${name}`;
    if (bytes + line.length + server.length + 2 > TOOL_MANIFEST_MAX_BYTES) { truncated = true; break; }
    if (!byServer.has(server)) byServer.set(server, []);
    byServer.get(server).push(line);
    bytes += line.length + 1;
  }
  if (!byServer.size) return "";
  const sections = [];
  for (const [server, srvLines] of byServer) sections.push("MCP server `" + server + "` — tools:\n" + srvLines.join("\n"));
  if (truncated) sections.push("…(more MCP tools available)");
  let out = "You have MCP tools available this turn, listed below by their MCP server. TREAT THESE MCP TOOLS AS FIRST-CLASS: use them exactly as you would your own built-in tools — they are never second-class, optional, or unavailable. The user (and your task) may refer to them plainly as 'tools', or by the action itself ('read the file', 'run a search', 'launch a workflow', 'use subagents') — those all mean these MCP tools, so CALL the matching MCP tool rather than doing the work yourself or claiming a listed tool is unavailable. Each is invoked as an MCP tool call (its MCP server + the tool name). Match each tool's parameter schema exactly (required fields, types, names); on an invalid-parameters error, fix the args and retry rather than abandon the tool.\n\n" + sections.join("\n\n");
  // Targeted clarifications (model-confirmed): Workflow lives on its MCP server (not top-level), so composer read
  // files itself; and its schema marks nothing required, so it omitted the script. Make both unambiguous, ONLY
  // when the tool is actually advertised this turn.
  const advNames = adv.map((t) => (t && (t.toolName || t.name)) || "");
  const notes = [];
  if (advNames.includes("Workflow")) {
    notes.push("To launch a workflow, invoke the `Workflow` MCP tool NOW — it lives on its MCP server (it is NOT a top-level tool or a feature of the codebase under review). Pass `script` (inline JS: `export const meta={...}` then agent()/parallel()/pipeline()) OR `scriptPath`; a name/title-only call with neither runs nothing. In the script `agent()` is POSITIONAL — `agent('prompt text', {agentType:'general-purpose'})`, NEVER `agent({prompt,...})` (an object makes the prompt '[object Object]'); and `parallel`/`pipeline` take `() =>` thunks, never bare `agent(...)`.");
  }
  const subagentTool = advNames.includes("Agent") ? "Agent" : advNames.find((n) => n === "Task" || n.indexOf("Task") === 0);
  if (subagentTool) {
    notes.push("To run work in subagents, actually invoke the `" + subagentTool + "` MCP tool with its arguments — do not merely narrate that you are delegating.");
  }
  if (notes.length) out += "\n\n" + notes.join("\n\n");
  return out;
}

// toolManifestRule wraps the manifest text in a valid always-apply agent.v1.CursorRule for delivery via
// requestContext.rules (the system-level, per-session channel). Proto shape is exact: { fullPath, content,
// type:{global:{}} (the "always apply" oneof), source, environments[] } — anything else fails the SDK's strict
// fromJson seam. Returns null when there is nothing to advertise.
function toolManifestRule(advertised, cwd) {
  const content = toolManifest(advertised);
  if (!content) return null;
  return {
    fullPath: (cwd || "/workspace") + "/.cursor/rules/cliproxy-tools.mdc",
    content, type: { global: {} }, source: "CURSOR_RULE_SOURCE_USER",
    environments: [], disabledEnvironments: [],
  };
}

// ---- session correlation: the global executor learns its session via AsyncLocalStorage ----
const als = new AsyncLocalStorage(); // store = { session }
// The patch reads this to advertise the client's tools (incl MCPs) as mcp_tools, per-session.
globalThis.__CC_GET_ADVERTISE__ = () => {
  const st = als.getStore();
  if (!st || !st.session) { console.warn("[bridge] __CC_GET_ADVERTISE__: no ALS session context; advertising no tools"); return []; }
  // ADD-40: consult the same turn-scoped effective set the dispatch gate uses (activeAdvertise when a run is
  // live), so what the model is OFFERED and what it can DISPATCH stay consistent under tool_choice.
  const adv = st.session.advertiseForGating ? st.session.advertiseForGating() : (st.session.advertise || []);
  // Proves the SDK's tool-advertising path (the non-prewarmed else branch) actually runs per model turn and
  // how many tools it hands the model. If this fires with a full count yet the model still calls only native
  // read/shell, the gap is the MODEL not engaging mcpTools — not a missing advertisement.
  dbg("__CC_GET_ADVERTISE__ called by SDK", "session=" + st.session.id, "returning=" + adv.length + " tools");
  return adv;
};

// wrapToolInput (ADD-104) gives a tool-call input a STABLE wire shape for the client. The SDK/model may emit
// MCP args as a raw string/number/array/boolean (or a client-registered tool whose JSON schema is a primitive),
// but the Go executor only maps OBJECT-shaped `input` into the client tool's arguments and collapses a
// non-object to {} — silently invoking the local tool with empty args. We wrap any non-plain-object value as
// {input:<raw>} BEFORE emitting the SSE tool_call, so a client that introspects always sees an object and the
// raw value survives under the pinned key 'input'. A plain object (or null/undefined) passes through unchanged
// (null/undefined render as {} downstream, which is the historical no-args shape).
function wrapToolInput(v) {
  if (v == null) return v;
  return (typeof v === "object" && !Array.isArray(v)) ? v : { input: v };
}

// Convert a proto Value/Struct/JSON-string into plain JSON (mcpArgs.args arrives as a proto map<string,Value>).
function toPlainJson(v) {
  if (v == null) return {};
  if (typeof v === "string") { try { return JSON.parse(v); } catch { return v; } }
  if (typeof v.toJson === "function") { try { return v.toJson(); } catch { /* fall through */ } }
  return v;
}

// Headless request context (never goes to CC): neutral /workspace paths, no sidecar dirs.
function requestContextEnvelope(ce, ws, cwd, rules) {
  return { __ccJson: { success: { requestContext: {
    rules,
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
// headlessRequestContext answers the SDK runtime's requestContextArgs CLIENT request, per session. When the tool
// manifest mode includes "rule", it injects an always-apply CursorRule carrying THIS session's tool manifest, so
// multi-client toolsets never collide (each session's rule reflects its own advertised set). clientEnv comes off
// the session; a null session (e.g. the startup self-test) yields the no-rules envelope.
function headlessRequestContext(session) {
  const ce = (session && session.clientEnv) || {};
  const ws = Array.isArray(ce.workspacePaths) && ce.workspacePaths.length ? ce.workspacePaths : ["/workspace"];
  const cwd = ce.processWorkingDirectory || ws[0] || "/workspace";
  let rules = [];
  if (TOOL_MANIFEST_IN_RULE && session) {
    const adv = (session.advertiseForGating && session.advertiseForGating()) || session.advertise || [];
    const rule = toolManifestRule(adv, cwd);
    if (rule) { rules = [rule]; dbg("toolManifest rule injected", "session=" + (session.id || "?"), "tools=" + adv.length, "bytes=" + rule.content.length); }
  }
  return requestContextEnvelope(ce, ws, cwd, rules);
}

// ── ExecServerMessage dispatch. EVERY exec case is routed, synthesized, or rejected — none falls through to
//    native sidecar execution. The AUTHORITATIVE classification is the live tables below, NOT a hand-maintained
//    prose list (a duplicated taxonomy drifted: it once still described mcpStateExecArgs as a fail-closed reject
//    after it became HEADLESS-synthesized, and listed grep/ls/fetch/etc. as rejects after they became
//    TYPED_UNAVAILABLE_U model-visible results). To classify a case, read: the `caseOf`-keyed branches in
//    __CC_EXEC_U / __CC_EXEC_S (requestContextArgs + mcpStateExecArgs are HEADLESS-synthesized; mcpArgs ->
//    dispatchMcp), CC_CASES (ROUTED→CC native fs/shell), CONTROL_ALLOW (precheck "allow"), CONTROL_TYPED
//    (proactive typed answer), and TYPED_UNAVAILABLE_U (model-visible typed-unavailable result). The actual
//    GUARANTEE is the deny-by-default fallback at the end of __CC_EXEC_U/__CC_EXEC_S: anything not in those
//    tables (incl. a future 30th case on an SDK bump) is REJECTED, never executed natively. selfTestResultSerialization
//    enumerates every emittable result shape straight from those tables (so it cannot drift); on an SDK bump,
//    verify the case set via the bundle's ExecServerMessage .fields.list() and re-run the self-tests.
//
// ccErrorMessageText renders an isError result's content into a single human-readable failure message for
// the `error: {error}` variant (the *Error oneof member's `error` string field; see fsErrorResult). The Go
// side may thread the failure reason as a plain string or a small structured object; either way the model
// must SEE the failure (C01), never a fabricated success shape.
function ccErrorMessageText(c, fallback) {
  if (typeof c === "string" && c.trim()) return c;
  if (c && typeof c === "object") { try { return JSON.stringify(c); } catch { /* fall through */ } }
  return fallback;
}

// ADD-57: the redacted workspace cwd reported in shell result metadata (workingDirectory / exit.cwd). The
// session's client env carries the user's REAL processWorkingDirectory (threaded by the Go executor via
// extractComposerClientEnv); fall back to the first workspace path, then the historical "/workspace" sentinel
// when no env is present. Reporting the actual cwd (instead of always "/workspace") keeps follow-up commands
// and relative paths anchored to the real directory rather than a fake one.
function composerWorkspaceCwd(clientEnv) {
  const ce = clientEnv || {};
  if (typeof ce.processWorkingDirectory === "string" && ce.processWorkingDirectory) return ce.processWorkingDirectory;
  if (Array.isArray(ce.workspacePaths) && ce.workspacePaths.length && typeof ce.workspacePaths[0] === "string" && ce.workspacePaths[0]) return ce.workspacePaths[0];
  return "/workspace";
}

// ADD-43: a native READ result is honest about completeness. The client tool may return EITHER a plain string
// (we have no completeness signal) OR a structured envelope {content, truncated, rangeApplied, totalLines,
// fileSize} when it actually applied an offset/limit/redaction. The old builder ALWAYS stamped
// truncated:false / rangeApplied:false and derived totalLines/fileSize from the returned text — so a partial
// excerpt was reported to the model as the COMPLETE file (it could then edit / conclude from missing context).
// Rule: when a structured envelope is present, PRESERVE its truncated/rangeApplied/totalLines/fileSize; when
// only a string is received AND the request asked for a bounded read (offset/limit present), we cannot prove
// completeness, so set truncated:true (degrade to "possibly partial" rather than claim a full read). An
// unbounded string read (no offset/limit) is treated as complete (truncated:false) — the historical behavior.
function buildReadSuccess(c, s) {
  // Structured envelope from the client tool: trust its completeness metadata.
  if (c && typeof c === "object") {
    const content = String(c.content ?? "");
    const out = { path: s && s.path, content };
    out.totalLines = c.totalLines != null ? String(c.totalLines) : String(content.split("\n").length);
    out.fileSize = c.fileSize != null ? String(c.fileSize) : String(Buffer.byteLength(content));
    out.truncated = Boolean(c.truncated);
    out.rangeApplied = c.rangeApplied != null ? Boolean(c.rangeApplied) : Boolean((s && (s.offset != null || s.limit != null)));
    return { success: out };
  }
  const content = String(c ?? "");
  // Plain string: a bounded read we cannot verify is COMPLETE -> mark possibly-truncated (do NOT claim full).
  const bounded = !!(s && (s.offset != null || s.limit != null));
  return { success: { path: s && s.path, content, totalLines: String(content.split("\n").length), fileSize: String(Buffer.byteLength(content)), truncated: bounded, rangeApplied: bounded } };
}

// ADD-43: a native WRITE result reports the ACTUAL post-write content/size when the client tool returns it
// (structured envelope {fileContentAfterWrite|content, linesCreated, fileSize}), instead of always echoing the
// REQUESTED fileText. A local write tool may normalize line endings, format, or partially write; reporting the
// requested text as the file content lets the model believe the file holds exactly what it asked for. When
// only a plain string (or nothing) is returned, fall back to the requested text but flag completeness honestly
// via returnFileContentAfterWrite semantics unchanged.
function buildWriteSuccess(c, s) {
  const requested = (s && s.fileText) || "";
  if (c && typeof c === "object") {
    const actual = c.fileContentAfterWrite != null ? String(c.fileContentAfterWrite) : (c.content != null ? String(c.content) : requested);
    const r = { success: { path: s && s.path } };
    r.success.linesCreated = c.linesCreated != null ? Number(c.linesCreated) : actual.split("\n").length;
    r.success.fileSize = c.fileSize != null ? String(c.fileSize) : String(Buffer.byteLength(actual));
    if (s && s.returnFileContentAfterWrite) r.success.fileContentAfterWrite = actual;
    return r;
  }
  // Plain string: if the client returned the post-write content as a string, prefer it; else use requested.
  const actual = typeof c === "string" && c ? c : requested;
  const r = { success: { path: s && s.path, linesCreated: actual.split("\n").length, fileSize: String(Buffer.byteLength(actual)) } };
  if (s && s.returnFileContentAfterWrite) r.success.fileContentAfterWrite = actual;
  return r;
}

// agent.v1.{Read,Write,Delete,Ls,Grep,Fetch,...}Error all share a `result.error` oneof member of shape
// { error: <message string>, ...optional context }. The failure text goes in the field literally named
// `error` — there is NO `message` field and NO generic agent.v1.Error{message} in @cursor/sdk 1.0.14
// (verified against the proto descriptors). Emitting { error: { message } } makes fromJson reject the result
// ("key \"message\" is unknown"), which would fail EVERY native read/write/delete failure at runtime (caught
// by selfTestResultSerialization / ADD-74). fsErrorResult builds the correct shape; `path` is included only
// when present (it is an optional scalar on the fs *Error types, absent on grep/fetch/etc.).
function fsErrorResult(message, path) {
  const e = { error: String(message == null ? "" : message) };
  if (path) e.path = String(path);
  return { error: e };
}

// Native Cursor tool cases routed to CC. ccTool = generic name (CLIProxy maps to the client's exact tool
// + arg schema). buildResult/buildChunks turn CC's tool_result content into the Cursor result toJson shape.
// C01/BR8: every buildResult takes (content, state, isError). When the client marked the tool result FAILED
// (isError), it MUST build a FAILURE shape — the result type's `error` oneof variant, which is { error:
// <message string>, path?: <path> } (NOT {message}; see fsErrorResult) — so a failed/cancelled/denied
// read/write/delete reaches the model AS a failure instead of a fabricated native success (which would let the
// model continue from a false filesystem state). ReadResult / WriteResult / DeleteResult each expose a
// `result` oneof with `success` plus `error`/`rejected`/typed error variants (verified against @cursor/sdk
// 1.0.14 proto descriptors); the patched `fromJson` builds whichever oneof key we emit. The shell cases already
// encode failure via a non-zero exitCode (their success variant is the protocol's failure channel), so they
// keep their existing exit-code handling.
// buildResult/buildChunks now take a trailing `ctx` ({ cwd }) so shell metadata reports the session's REAL
// working directory (ADD-57) instead of a hard-coded "/workspace". Read/write delegate to the ADD-43 honest
// builders (preserve structured truncated/range/actual-content; degrade to truncated:true for an unverifiable
// bounded string read; never fabricate full-file success).
const CC_READ_CASE = { ccTool: "read", stream: false, buildResult: (c, s, isError) => isError ? fsErrorResult(ccErrorMessageText(c, "read failed"), s && s.path) : buildReadSuccess(c, s) };
const CC_CASES = {
  readArgs: CC_READ_CASE,
  redactedReadArgs: CC_READ_CASE,
  writeArgs:       { ccTool: "write", stream: false, buildResult: (c, s, isError) => isError ? fsErrorResult(ccErrorMessageText(c, "write failed"), s && s.path) : buildWriteSuccess(c, s) },
  // agent.v1.DeleteSuccess.deleted_file is a STRING scalar (not bool) and file_size is fabricated when the
  // client returns no metadata, so we omit it rather than assert a false "0" size; deletedFile:"true" conveys
  // the deletion in the correct type (a bool/number is rejected by fromJson).
  deleteArgs:      { ccTool: "delete", stream: false, buildResult: (c, s, isError) => isError ? fsErrorResult(ccErrorMessageText(c, "delete failed"), s && s.path) : ({ success: { path: s && s.path, deletedFile: "true" } }) },
  // BR8/C5: when the client marked the native shell result as failed (isError) but the parsed exitCode is 0,
  // force a non-zero exit so the model sees the failure (a success exit would mask a failed/cancelled tool).
  // ADD-57: report ctx.cwd (the session's real processWorkingDirectory) in workingDirectory / exit.cwd.
  shellArgs:       { ccTool: "shell", stream: false, buildResult: (c, s, isError, ctx) => { const r = parseShellContent(c); const code = isError && r.exitCode === 0 ? 1 : r.exitCode; return { success: { command: s && s.command, workingDirectory: (ctx && ctx.cwd) || "/workspace", exitCode: code, stdout: r.stdout, stderr: r.stderr } }; } },
  shellStreamArgs: { ccTool: "shell", stream: true,  buildChunks: (c, isError, ctx) => { const r = parseShellContent(c); const code = isError && r.exitCode === 0 ? 1 : r.exitCode; const aborted = isError ? true : r.aborted; const out = [{ stdout: { data: r.stdout } }]; if (r.stderr) out.push({ stderr: { data: r.stderr } }); out.push({ exit: { code, cwd: (ctx && ctx.cwd) || "/workspace", aborted, localExecutionTimeMs: 1 } }); return out; } },
  // grep/ls: routed via TYPED_UNAVAILABLE_U in unaryExecPreflight (not CC_CASES).
};
// Control-flow exec cases the server may send: answer with a typed "allow" so the run proceeds (a bare
// error reject can deny the action / desync). allowlisted is a bool.
const CONTROL_ALLOW = { shellAllowlistPrecheckArgs: 1, mcpAllowlistPrecheckArgs: 1, webFetchAllowlistPrecheckArgs: 1 };
// Server-proactive cases that may fire at turn start: answer with a benign TYPED result so the run
// proceeds — a bare Error throw is a plausible desync/ERROR_BAD_REQUEST vector. If a shape is wrong,
// fromJson throws and it degrades to the same exec error (no worse than rejecting).
// diagnosticsArgs -> typed "rejected" (DiagnosticsResult HAS a rejected variant). canvasDiagnosticsArgs ->
// "success" with empty diagnostics: CanvasDiagnosticsResult exposes ONLY success/error (no rejected member),
// so a "rejected" shape fails fromJson — an empty success ("checked, found nothing") is the benign typed
// answer (verified against the @cursor/sdk 1.0.14 descriptors + selfTestResultSerialization).
// TODO(validate-live): smartModeClassifierArgs needs its success shape (default mode) + subagent* a typed
// error; left as deny-by-default reject until their shapes are derived and exercised against live Cursor.
const CONTROL_TYPED = { diagnosticsArgs: { rejected: {} }, canvasDiagnosticsArgs: { success: {} } };
// H17: cases the bridge does NOT implement but whose RESULT type exposes an `error` oneof variant whose member
// is { error: <message string>, ...optional context } — NOT {message} (see fsErrorResult/typedUnavailableResult;
// there is NO agent.v1.Error{message}). The shape is pinned at startup by selfTestResultSerialization driving
// typedUnavailableResult through the real fromJson seam (check #6) — reference THAT, not an unverifiable proto
// descriptor claim. For these we return a MODEL-VISIBLE typed unavailable result instead of fail-closing the
// whole run with a stream error — so the model sees "this tool is unavailable" and picks another path (e.g.
// shell instead of structured grep/ls).
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
  // NOTE: mcpStateExecArgs is deliberately NOT here — it is the runtime's MCP-inventory query and is answered
  // with a real McpStateSuccess (headlessMcpState). Answering it with a typed error made the backend offer the
  // model ZERO MCP tools (dispatchMcp=0) even though the loopback servers were dialed + tools/list'd.
]);
function typedUnavailableResult(cas) {
  // Each TYPED_UNAVAILABLE_U case maps to a <X>Result whose `error` oneof member is { error: <string>, ... }
  // (grep/ls/fetch/backgroundShellSpawn/writeShellStdin/listMcpResources/readMcpResource/mcpState — all carry
  // an `error` string field; extra context fields are optional). The message goes in `error`, never `message`.
  return { __ccJson: { error: { error: `tool '${cas}' is not available in this environment; use an alternative approach` } } };
}

// mcpDispatchResult builds the McpResult `success` wrap the MCP dispatch path emits — { success: { isError,
// content: [{ text: { text } }] } } (agent.v1.McpResult.success: McpToolResult). It is the SHARED builder for
// dispatchMcp's resolved result AND the handleMcp "tool not advertised" wrap, so selfTestResultSerialization can
// drive the SAME function the live run uses instead of a hand-retyped literal (ADD-74: a literal can drift from
// the real shape and pass CI while the first real tool-call crashes inside fromJson). content is normalized to a
// string exactly as the live wrap did (object content -> JSON.stringify). isError is strict-true.
function mcpDispatchResult(content, isError, images) {
  const text = typeof content === "string" ? content : JSON.stringify(content ?? "");
  const parts = [];
  // EX3 (clean path A): inline base64 tool-result images as McpImageContent — the content oneof's `image`
  // member (agent.v1.McpImageContent = { data: bytes(T:12), mime_type: string }; protobuf-es fromJson decodes a
  // base64 STRING into the bytes field). Placed BEFORE the text part. url-form images carry no base64 `data`
  // here, so they are skipped and fall back to the (flag-off) fresh-send path. Caller gates whether images flow.
  if (Array.isArray(images)) {
    for (const im of images) {
      if (im && typeof im.data === "string" && im.data && typeof im.mimeType === "string" && im.mimeType) {
        parts.push({ image: { data: im.data, mimeType: im.mimeType } });
      }
    }
  }
  if (text || parts.length === 0) parts.push({ text: { text } });
  return { success: { isError: isError === true, content: parts } };
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
function blockedNativeResult(cas, s, cwd = "/workspace") {
  switch (cas) {
    case "shellArgs":
      // ADD-57: report the session's real cwd here too (cosmetic — nothing executed — but consistent).
      return { __ccJson: { success: { command: s && s.command, workingDirectory: cwd, exitCode: 1, stdout: "", stderr: NATIVE_TOOL_DISABLED_MSG } } };
    default:
      // read/write/delete/redactedRead -> the result type's `error` oneof variant { error: <string>, path? }.
      return { __ccJson: fsErrorResult(NATIVE_TOOL_DISABLED_MSG, s && s.path) };
  }
}
function caseOf(t) { return t && t.message && t.message.case; }

// ---- the patched bundle calls these (deny-by-default; never native) ----
function unaryExecPreflight(cas, store) {
  if (cas === "requestContextArgs") return Promise.resolve(headlessRequestContext(store && store.session));
  // The runtime's MCP-inventory query: answer with the session's advertised tools as connected loopback
  // servers so the backend exposes them to the model (see headlessMcpState). Without this the model gets zero
  // MCP tools. Checked before TYPED_UNAVAILABLE_U (which no longer lists mcpStateExecArgs).
  if (cas === "mcpStateExecArgs") return Promise.resolve(headlessMcpState(store && store.session));
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
  return null;
}

function blockedNativeExecIfNeeded(store, cas, s, stream) {
  if (!store || !nativeToolBlockedByChoice(store.session.toolChoice)) return null;
  const cwd = composerWorkspaceCwd(store.session.clientEnv);
  dbg("__CC_EXEC_" + (stream ? "S" : "U") + " native tool blocked by tool_choice", "session=" + store.session.id, "cas=" + cas, "toolChoice=" + store.session.toolChoice);
  if (stream) {
    return (async function* () { yield { __ccJson: { stdout: { data: "" } } }; yield { __ccJson: { exit: { code: 1, cwd, aborted: true, localExecutionTimeMs: 1 } } }; })();
  }
  return Promise.resolve(blockedNativeResult(cas, s, cwd));
}

globalThis.__CC_EXEC_U = function (n, e, s, t) {
  const cas = caseOf(t);
  const store = als.getStore();
  const preflight = unaryExecPreflight(cas, store);
  if (preflight) return preflight;
  if (cas === "mcpArgs") {
    if (!store) return Promise.reject(new Error("[bridge] mcpArgs outside a session"));
    return store.session.dispatchMcp(s);
  }
  const spec = CC_CASES[cas];
  if (!spec || spec.stream || !spec.buildResult || !store) {
    return Promise.reject(new Error(`[bridge] tool '${cas}' not supported by the Claude Code bridge`));
  }
  const blocked = blockedNativeExecIfNeeded(store, cas, s, false);
  if (blocked) return blocked;
  return store.session.dispatchUnary(cas, spec, s);
};
globalThis.__CC_EXEC_S = function (n, e, s, t) {
  const cas = caseOf(t);
  const spec = CC_CASES[cas];
  const store = als.getStore();
  if (!spec || !spec.stream || !spec.buildChunks || !store) {
    return (async function* () { throw new Error(`[bridge] streaming tool '${cas}' not supported by the Claude Code bridge`); })();
  }
  const blocked = blockedNativeExecIfNeeded(store, cas, s, true);
  if (blocked) return blocked;
  return store.session.dispatchStream(cas, spec, s);
};

// ---- session: holds the live SDK run + bridges tool calls across /agent/turn calls ----
const sessions = new Map();
// Bound the sessions map (no unbounded growth): evict least-recently-active, IDLE sessions over the cap. A
// session is idle-evictable ONLY when it has no open response, no live/paused run, AND no queued waiters — so
// we never evict a session that is streaming or paused awaiting tool_results (its SDK run is live between
// HTTP turns) or has work queued behind it.
function enforceSessionCap() {
  if (sessions.size <= MAX_SESSIONS) return;
  const evictable = [...sessions.values()].filter((s) => !s.activeRes && !s.run && !s.hasQueuedWaiters()).sort((a, b) => a.lastActivity - b.lastActivity);
  for (const s of evictable) { if (sessions.size <= MAX_SESSIONS) break; sessions.delete(s.id); void s.cancel(); }
}
// ADD-63 (LOAD-SHED, never evict live work): before admitting a NEW session, try to make room by evicting
// idle sessions, then report whether there is room. When the map is at MAX_SESSIONS and EVERY session is
// active/paused/waitered (nothing idle to evict), this returns false and handleTurn rejects the new session
// with a typed 429 — we NEVER evict a live/paused session out from under in-flight work to admit a new one.
function sessionCapHasRoomForNew() {
  if (sessions.size < MAX_SESSIONS) return true;
  enforceSessionCap();                 // shed idle sessions if any
  return sessions.size < MAX_SESSIONS;  // room only if shedding freed a slot
}
// Per-Cursor-key platform pool. Single-tenant: one entry keyed by API_KEY with stateRoot = STATE_ROOT
// (NOT namespaced, so existing durable sessions survive an upgrade). Multi-tenant: one platform per
// forwarded key, each with an isolated stateRoot STATE_ROOT/k_<hash>, so distinct users never share a
// Cursor account or durable state. Bounded (MAX_PLATFORMS) + idle-evicted so the pool can't grow without limit.
const platforms = new Map(); // keyHash -> { promise, stateRoot, lastUsed, fp }
function keyHash(k) { return createHash("sha256").update(String(k || "")).digest("hex").slice(0, 16); }
// ADD-53: the FULL sha-256 digest of the key. The platform map / on-disk stateRoot dir keep the 16-hex
// truncated name (renaming k_<hash> would orphan existing durable SDK state), but each entry now ALSO stores
// the full fingerprint so two keys that collide on the first 64 bits cannot silently share a platform/account
// + durable state — a truncated-collision-but-different-full-key is rejected in getPlatform/platformHasSession.
function keyFingerprint(k) { return createHash("sha256").update(String(k || "")).digest("hex"); }
function platformStateRoot(h) { return MULTI_TENANT ? path.join(STATE_ROOT, "k_" + h) : STATE_ROOT; }
// PlatformKeyCollisionError marks a 64-bit truncated-hash collision between two DIFFERENT Cursor keys (ADD-53);
// handleTurn maps it to a 500 rather than running under the wrong account.
class PlatformKeyCollisionError extends Error { constructor(msg) { super(msg); this.code = "PLATFORM_KEY_COLLISION"; } }
function getPlatform(cursorKey) {
  const h = keyHash(cursorKey);
  const fp = keyFingerprint(cursorKey);
  let entry = platforms.get(h);
  if (entry) {
    // ADD-53: same truncated hash but a DIFFERENT full key -> a genuine collision. Never reuse the first key's
    // platform/account/state for the second key; fail loud so the request is rejected, not mis-attributed.
    if (entry.fp && entry.fp !== fp) {
      dbg("getPlatform KEY HASH COLLISION (different full key) -> reject", "hash=" + h);
      throw new PlatformKeyCollisionError("cursor key hash collision: two distinct keys share a 64-bit platform hash; refusing to share durable state");
    }
  } else {
    const stateRoot = platformStateRoot(h);
    try { mkdirSync(stateRoot, { recursive: true }); } catch { /* createAgentPlatform will surface a real error */ }
    // ADD-61: do NOT cache a REJECTED createAgentPlatform promise — a transient init failure (sqlite open,
    // FS blip, SDK init) would otherwise poison this tenant until restart (every later turn reuses the same
    // rejected promise + the pinned entry blocks idle eviction). Evict the entry on reject so the next turn
    // retries cleanly. The .catch re-throws so the awaiting caller still sees the real error this turn.
    const promise = loadSdk().createAgentPlatform({ apiKey: cursorKey, stateRoot })
      .catch((e) => { if (platforms.get(h) === entry) platforms.delete(h); throw e; });
    entry = { promise, stateRoot, lastUsed: nowMs(), fp };
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
// ADD-75 (LOAD-SHED mirror of ADD-63 for the platform pool): before admitting a NEW tenant (a key with no
// existing platform), report whether there is room. An EXISTING key reuses its platform (always room). When
// the pool is at MAX_PLATFORMS and every platform is pinned (platformHasSession), there is nothing to evict —
// return false so handleTurn rejects the new tenant with a typed 429 rather than growing past the cap and
// exhausting fds / sqlite handles / memory. We never dispose a pinned (live/paused) tenant's platform.
function platformCapHasRoomForNew(cursorKey) {
  const h = keyHash(cursorKey);
  if (platforms.has(h)) return true;        // existing tenant: reuses its platform, no new entry
  if (platforms.size < MAX_PLATFORMS) return true;
  enforcePlatformCap();                      // shed idle (unpinned) platforms if any
  return platforms.size < MAX_PLATFORMS;
}

// ---- Upstream rate-limit hardening (NGHTTP2_ENHANCE_YOUR_CALM) ----
// When Cursor's HTTP/2 gateway flood-protects an account it RST_STREAMs with NGHTTP2_ENHANCE_YOUR_CALM. The SDK
// holds ONE persistent HTTP/2 connection per platform (the getPlatform cache), so once that connection is
// flagged EVERY reused stream on it fails the same way until the connection is recycled — and getPlatform only
// evicts a REJECTED create (ADD-61), never a successfully-created platform whose connection later got poisoned.
// These helpers close that gap: classify the signal, recycle the poisoned connection, and run a per-key circuit
// breaker so client retries back off instead of immediately re-poisoning the freshly-dialed connection.

// isUpstreamRateLimit detects Cursor's HTTP/2 flood/rate-limit signal and adjacent transport codes. The error
// arrives as a @connectrpc ConnectError whose message carries the nghttp2 code. Exported for tests.
function isUpstreamRateLimit(reason) {
  if (!reason) return false;
  if (reason.code === "resource_exhausted") return true;
  const msg = (reason.message != null ? String(reason.message) : (typeof reason === "string" ? reason : ""));
  return /ENHANCE_YOUR_CALM|RESOURCE_EXHAUSTED|too many requests|rate.?limit/i.test(msg);
}

// recyclePlatform evicts + disposes the cached platform (and its poisoned HTTP/2 connection) for a key hash so
// the NEXT turn dials a FRESH connection with a clean stream budget. Best-effort dispose (fire-and-forget).
function recyclePlatform(h) {
  const entry = platforms.get(h);
  if (!entry) return false;
  platforms.delete(h);
  void disposePlatform(entry);
  return true;
}

// Per-key circuit breaker for upstream rate-limiting. While OPEN (now < openUntil) handleTurn fast-fails NEW
// runs for that key with a clear 429 so the client backs off; the window grows exponentially (capped) per
// consecutive trip. A successful run closes it (closeBreaker in onRunComplete). This is an IN-PROCESS,
// PRE-CONNECT rate guard (it bounds how often we re-dial) — NOT a timeout on an established upstream stream, so
// it stays in the allowed class alongside the abandonment guards in AGENTS.md.
const CURSOR_RATELIMIT_BASE_MS = envInt("CURSOR_COMPOSER_RATELIMIT_BASE_MS", 4000, { min: 100 });
const CURSOR_RATELIMIT_MAX_MS = envInt("CURSOR_COMPOSER_RATELIMIT_MAX_MS", 60000, { min: 1000 });
const upstreamBreaker = new Map(); // keyHash -> { fails, openUntil }

function breakerBackoffMs(fails) {
  const f = Math.max(1, fails);
  return Math.min(CURSOR_RATELIMIT_MAX_MS, CURSOR_RATELIMIT_BASE_MS * Math.pow(2, f - 1));
}
function tripBreaker(h, now = nowMs()) {
  const e = upstreamBreaker.get(h) || { fails: 0, openUntil: 0 };
  e.fails += 1;
  e.openUntil = now + breakerBackoffMs(e.fails);
  upstreamBreaker.set(h, e);
  return e;
}
function breakerOpen(h, now = nowMs()) {
  const e = upstreamBreaker.get(h);
  return !!(e && now < e.openUntil);
}
function breakerRetryAfterMs(h, now = nowMs()) {
  const e = upstreamBreaker.get(h);
  return e ? Math.max(0, e.openUntil - now) : 0;
}
function closeBreaker(h) {
  return upstreamBreaker.delete(h);
}

// soleStreamingSession returns the ONE session with an in-flight streaming run (run set, activeRes open, not
// done), or null when 0 or 2+ qualify — used to safely attribute a floating rejection that carries no session
// handle. Shared by the input-stream-closed teardown and the rate-limit attribution.
function soleStreamingSession(sessionsMap) {
  if (!sessionsMap || typeof sessionsMap.values !== "function") return null;
  let victim = null;
  for (const s of sessionsMap.values()) {
    if (s && s.run && s.activeRes && !s.done) { if (victim) return null; victim = s; }
  }
  return victim;
}

// rateLimitedKeyToRecycle picks the key hash whose connection to recycle on an ENHANCE_YOUR_CALM rejection that
// carries no key. Single-tenant (the common case) — exactly one platform — is unambiguous. Otherwise attribute
// via the lone in-flight session; if still ambiguous (2+ tenants mid-run), return null (log-only) rather than
// recycle the wrong tenant's healthy connection.
function rateLimitedKeyToRecycle(sessionsMap, platformsMap) {
  if (platformsMap && platformsMap.size === 1) return [...platformsMap.keys()][0];
  const s = soleStreamingSession(sessionsMap);
  return s ? keyHash(s.cursorKey) : null;
}

class Session {
  constructor(id, cursorKey) {
    this.id = id;
    // #8 (review): a short per-session hash used to NAMESPACE client-visible tool-call ids (wireToolId), so two
    // sibling sessions emitting the same sanitized local id produce GLOBALLY-unique visible ids — the Go
    // ownership map then keys on a unique id per (session, tool) and never falsely flags cross-session ambiguity.
    this.idHash = createHash("sha256").update(String(id)).digest("hex").slice(0, 8);
    // C05: the DURABLE Cursor agent id is DECOUPLED from the external sessionID. `id` is the stable routing
    // key the Go executor derives (continuations keep routing here); `agentId` is what we hand to
    // resumeAgent/createAgent. They start equal, but on ERROR_CONVERSATION_TOO_LONG we ROTATE `agentId`
    // (e.g. <id>_r2) and tombstone the poisoned durable agent, so the next turn seeds a FRESH agent under a
    // new id and never resumeAgent()s the over-budget one — while the external sessionID is unchanged.
    this.agentId = id;
    this.recoveryEpoch = 0;       // C05: increments each too-long rotation; suffixes the rotated agentId
    this.modelEpoch = 0;          // ADD-62: increments each MODEL-CHANGE rotation; a SEPARATE budget from
                                  // recoveryEpoch so toggling models never burns the crash-recovery rotations
    this.keyEpoch = 0;            // ADD-79: increments each CURSOR-KEY-CHANGE rotation; a SEPARATE budget so a
                                  // key rotation never burns the crash-recovery / model-change rotations. A turn
                                  // whose upstream Cursor key fingerprint differs from this session's tombstones
                                  // the durable agent (bound to the old account) + seeds a fresh agent under the
                                  // new key, instead of silently continuing on the old (possibly revoked) account.
    this.model = null;            // ADD-62: the model the durable agent was created/resumed under. A turn that
                                  // requests a DIFFERENT model rotates the durable agent (the old agent is bound
                                  // to the old model) + forces a re-seed, instead of silently answering from it.
    this.cursorKey = cursorKey || API_KEY; // the Cursor key whose platform this session runs on
    this.agent = null; this.agentPromise = null; this.run = null;
    this.activeRes = null; this.pending = new Map();
    this.turnBatch = []; this.flushTimer = null;
    this.stepToolStarted = 0;     // tool-call-started deltas seen for the CURRENT assistant step (the step's tool
                                  // count); used to wait for a slow batch before pausing. Reset at step/turn boundaries.
    this.batchWaitExtensions = 0; // how many extra debounce windows we have waited for the current step's batch
                                  // (bounded by TOOL_BATCH_MAX_EXTENSIONS so an over-count never hangs the turn).
    this.delivered = new Set();   // tool ids the client has SEEN (sent in a turn_end) this logical run — so a
                                  // tool_results turn matches against what was actually delivered (Comment 2)
    this.everEmitted = new BoundedIdSet(); // BR1/ADD-49: tool ids this session has issued to the client, across
                                  // the session lifetime (NOT cleared per run like `delivered`), BOUNDED to the
                                  // most-recent EVER_EMITTED_CAP ids (LRU). A late tool_result for an id we DID
                                  // recently emit (then watchdog-reaped / already resolved) is benign; an id
                                  // never issued — OR one so old it aged out of the bound — is unknown -> error
                                  // turn. Bounding stops both unbounded memory growth and the permanent-benign
                                  // downgrade of ancient ids on long-lived sessions.
    this.undelivered = [];        // {id,name,input} of tools emitted with no open response (turn closed mid-burst);
                                  // delivered on the next /agent/turn so the client can answer them (Comment 1)
    this.rawToWireId = new Map();  // H23: raw SDK tool-call id -> the sanitized WIRE id we emitted for it. Same
                                   // raw id always maps to the same wire id (idempotent); two DIFFERENT raw ids
                                   // that sanitize to the same wire id get DISAMBIGUATED so neither overwrites
                                   // the other's pending (the original collision bug at pending.set).
    this.usedWireIds = new Set();  // H23: every wire id this session has handed out (collision detection set).
    this.turnToken = 0;           // increments per turn; flush is bound to a token
    this.settleTurn = null;
    this.stashedToolResultImages = []; // EX3: tool-result images from a PARTIAL batch, held until the batch completes
    this.streamedText = "";       // cumulative text streamed in the CURRENT run (reset per user turn)
    this.reasonedThisRun = false; // #15: whether the CURRENT run emitted any reasoning (reset per user turn).
                                  // Reasoning counts as produced output, so a reasoning-only finished run is not
                                  // mis-flagged as an empty turn (false error) by onRunComplete.
    this.pendingDeltas = [];      // #14: ordered {type:'text'|'reasoning',delta} the run produced while NO
                                  // response was open (it outlived the turn). Flushed IN ORDER on the next turn's
                                  // open response instead of being DROPPED (text deltas used to vanish here while
                                  // only tool-uses were buffered in `undelivered`). Cleared at clearTurnState.
    this.pendingDeltaBytes = 0;   // running byte size of pendingDeltas (bounded like outQueue)
    this.advertise = [];
    this.activeAdvertise = null;  // ADD-40: the TURN-SCOPED effective advertised set (gated by tool_choice) for
                                  // the LIFETIME of the live run. `advertise` is restored to the full set right
                                  // after agent.send (it is needed for cross-turn refresh + re-seed), but the
                                  // MODEL's MCP dispatch (dispatchMcp/reconcileToolName/__CC_GET_ADVERTISE__)
                                  // happens ASYNC later in the run — it must consult THIS set, not the restored
                                  // full inventory, or a tool_choice:none/specific run could still dispatch a
                                  // disallowed tool. Set when the run starts; cleared on run complete/error/cancel.
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
    // ADD-100: SSE backpressure/failure state. `sse()` returns a boolean (true=accepted/queued, false=dropped
    // because the turn was already torn down by overflow). When res.write() signals backpressure (returns false)
    // we buffer the payload in `outQueue` (BOUNDED) and drain it on the socket's 'drain' event; a thrown write
    // marks the write path failed. If the queue exceeds OUT_QUEUE_MAX_BYTES (a slow/stuck client that never
    // drains) we cancel the turn with a typed transport error rather than letting Node buffer unboundedly. The
    // 'drain' listener is attached lazily (once) per activeRes and detached on clearTurnState/cancel.
    this.outQueue = [];              // queued SSE payload strings awaiting 'drain'
    this.outQueueBytes = 0;          // running byte size of outQueue (cap enforcement)
    this.writeFailed = false;        // a write threw OR the queue overflowed -> the stream is dead for this turn
    this._drainBound = null;         // the bound 'drain' handler attached to the current activeRes (for detach)
    this._drainRes = null;           // the res the 'drain' handler is attached to (so we detach from the right one)
    this._logicalDone = [];          // resolvers fired when the live run TRULY completes (onRunComplete/onRunError/cancel), NOT at a tool-pause
    this.runEpoch = 0;               // bumped per run + on cancel; a run.wait() callback ignores its result if the epoch advanced (the run was superseded/cancelled and a new turn may already own the session)
    // ADD-106 (Comment 3): per-LOGICAL-RUN agentic-loop bound counters. Reset by resetLoopBounds() when a fresh
    // send starts a new logical run; advanced once per tool-result round (pauseForTools). loopTripped latches so
    // the bound is enforced exactly once per run (the run is terminated as turn_end{error}, never a clean end).
    this.toolRounds = 0;             // tool-result rounds taken by the CURRENT logical run (pauseForTools cycles)
    this.lastToolSig = null;         // signature (name+args) of the SOLE tool in the last single-tool round, or null
    this.repeatToolCount = 0;        // consecutive single-tool rounds whose signature equals lastToolSig
    this.loopTripped = false;        // latched true once a loop bound trips, so we terminate the run only once
  }

  touch() { this.lastActivity = nowMs(); }
  hasQueuedWaiters() { return this.waiters > 0; }
  resetSeedState() { this.seeded = false; this.seededSystem = ""; this.historyFingerprint = null; }
  async finishRotationCancel() { await this.cancel(); this.done = false; }
  // whenLogicalDone resolves when the CURRENT live run terminates. If no run is live, it resolves now. This
  // is the queue's admission signal and is deliberately DISTINCT from settle(): settle() also fires when a
  // turn pauses for client tools while the SDK run stays alive awaiting tool_results, so admitting the next
  // turn on settle() would collide with the still-live run. Admission must wait for real completion.
  whenLogicalDone() { if (!this.run) return Promise.resolve(); return new Promise((r) => this._logicalDone.push(r)); }
  notifyLogicalDone() { const ws = this._logicalDone; this._logicalDone = []; for (const w of ws) { try { w(); } catch {} } }
  // sse writes an SSE frame and returns whether the WRITE PATH is healthy (ADD-100). Returns true when the
  // frame was accepted by the socket OR safely queued behind backpressure; returns false when the write threw
  // or the per-session output queue overflowed (the turn is then cancelled). Callers that gate delivery on the
  // write succeeding (emitToolUse / flushUndelivered / pauseForTools) MUST check this and re-buffer / not pause
  // on false, so a tool is never marked `delivered` to a client that did not actually receive it.
  sse(obj) {
    if (!this.activeRes) return false;
    if (this.writeFailed) return false;
    return this.writePayload(formatSseData(obj));
  }
  // writePayload performs the low-level write with backpressure handling. true = accepted or queued; false =
  // failed (threw) or overflowed. On a `false` return from res.write() (backpressure) we queue the payload and
  // attach a one-shot 'drain' listener (idempotent per res) to flush the queue when the socket clears.
  writePayload(payload) {
    if (this.writeFailed || !this.activeRes) return false;
    const res = this.activeRes;
    // If we are already buffering behind backpressure, keep order: queue rather than writing ahead of the queue.
    if (this.outQueue.length) return this.enqueueOut(payload);
    let ok;
    try { ok = res.write(payload); }
    catch (e) { dbg("sse write THREW -> mark stream dead + cancel turn (ADD-100)", "session=" + this.id, (e && e.message) || String(e)); this.failWrite("sse write error: " + ((e && e.message) || String(e))); return false; }
    if (ok === false) return this.enqueueOut(payload);
    return true;
  }
  // enqueueOut buffers a payload that backpressure rejected and ensures the 'drain' flusher is attached. Returns
  // false (and cancels the turn) when the bounded queue would overflow; true when safely queued.
  enqueueOut(payload) {
    const bytes = Buffer.byteLength(payload);
    if (this.outQueueBytes + bytes > COMPOSER_OUT_QUEUE_MAX_BYTES) {
      dbg("sse outQueue overflow -> cancel turn (ADD-100)", "session=" + this.id, "queuedBytes=" + this.outQueueBytes, "cap=" + COMPOSER_OUT_QUEUE_MAX_BYTES);
      this.failWrite(`SSE output queue exceeded ${COMPOSER_OUT_QUEUE_MAX_BYTES} bytes (client backpressure)`);
      return false;
    }
    this.outQueue.push(payload);
    this.outQueueBytes += bytes;
    this.attachDrain();
    return true;
  }
  // attachDrain wires a single 'drain' listener on the current activeRes that flushes outQueue in order.
  attachDrain() {
    const res = this.activeRes;
    if (!res || (this._drainBound && this._drainRes === res) || typeof res.on !== "function") return;
    this.detachDrain();
    this._drainRes = res;
    this._drainBound = () => this.drainOut();
    res.on("drain", this._drainBound);
  }
  detachDrain() {
    if (this._drainBound && this._drainRes && typeof this._drainRes.off === "function") {
      try { this._drainRes.off("drain", this._drainBound); } catch { /* ignore */ }
    }
    this._drainBound = null; this._drainRes = null;
  }
  // drainOut flushes the queued payloads on a 'drain' event, stopping if the socket signals backpressure again.
  drainOut() {
    const res = this.activeRes;
    if (!res || this.writeFailed) return;
    while (this.outQueue.length) {
      const payload = this.outQueue[0];
      let ok;
      try { ok = res.write(payload); }
      catch (e) { this.failWrite("sse drain write error: " + ((e && e.message) || String(e))); return; }
      this.outQueue.shift();
      this.outQueueBytes -= Buffer.byteLength(payload);
      if (ok === false) return; // still backpressured; the next 'drain' resumes
    }
    this.detachDrain();
  }
  // failWrite marks the write path dead and cancels the turn with a typed transport error so the run is torn
  // down (rejecting pendings, advancing the FIFO) instead of waiting on a client that will never receive output.
  failWrite(reason) {
    if (this.writeFailed) return;
    this.writeFailed = true;
    this.outQueue = []; this.outQueueBytes = 0;
    this.detachDrain();
    this.lastRunError = "[bridge] SSE transport failure: " + reason;
    // Cancel asynchronously; cancel() rejects pendings + notifies logical-done so the queue advances. We do NOT
    // try to write an error frame (the stream is dead). settle() (inside cancel) clears this turn's latch.
    void this.cancel();
  }

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
    if (!raw) return this.namespaceToolId(ccToolId(s)); // minted uuid path: unique by construction
    const existing = this.rawToWireId.get(raw);
    if (existing) return existing; // same raw id -> same (already-namespaced) wire id (idempotent re-emit)
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
    this.usedWireIds.add(wire); // tracks the LOCAL sanitized id for per-session collision detection
    // #8 (D4/review): namespace the VISIBLE id with this session so it is GLOBALLY unique at the tenant boundary.
    // The pending/delivered/everEmitted sets and resolvePending all use this SAME namespaced id, so the
    // emit -> client -> tool_results round-trip is transparent; only the SDK's internal raw id differs, and the
    // bridge never needs the raw id to route a result back (the pending is keyed by the namespaced id). The Go
    // executor forwards the namespaced id verbatim, so its tenant-global ownership map keys on a unique id.
    const namespaced = this.namespaceToolId(wire);
    this.rawToWireId.set(raw, namespaced);
    return namespaced;
  }
  // namespaceToolId prefixes a per-session short hash so the client-visible tool id is unique across ALL of the
  // tenant's concurrent sessions (cct_<sessionHash>_<localId>). Idempotent if already namespaced (defensive).
  namespaceToolId(localWire) {
    const prefix = `cct_${this.idHash}_`;
    return String(localWire).startsWith(prefix) ? String(localWire) : prefix + localWire;
  }

  // ADD-76: create the pending entry WITHOUT arming the abandonment watchdog. The timer must NOT start until
  // the tool is actually DELIVERED to the client (a tool buffered in `undelivered` with no open response has
  // never been seen, so reaping it after PENDING_TIMEOUT_MS would reject a tool the client never had a chance
  // to answer). startPendingTimer arms the watchdog at delivery (pauseForTools / flushUndelivered). A pending
  // whose timer never started simply has timer:null; clearTimeout(null) and clearTimeout(undefined) are no-ops.
  newPending(id, resolveWrap) {
    this.pending.set(id, { resolve: resolveWrap, reject: (err) => { const p = this.pending.get(id); if (p && p.timer) clearTimeout(p.timer); resolveWrap.__reject(err); }, timer: null });
  }
  // startPendingTimer arms the PENDING_TIMEOUT_MS abandonment watchdog for an ALREADY-DELIVERED tool (ADD-76).
  // Idempotent: re-arming an already-armed (or already-resolved) pending is a no-op, so calling it from both
  // pauseForTools and flushUndelivered for the same id never double-arms.
  startPendingTimer(id) {
    const p = this.pending.get(id);
    if (!p || p.timer) return;
    p.timer = setTimeout(() => {
      const cur = this.pending.get(id);
      if (cur) { this.pending.delete(id); cur.reject(new Error(`[bridge] tool ${id} abandoned after ${PENDING_TIMEOUT_MS}ms`)); }
    }, PENDING_TIMEOUT_MS);
  }
  // resolvePending answers a pending client tool call. isError (C5/BR4) flags a FAILED/cancelled client tool
  // so the result reaches the model AS a failure (the resolve wrappers in dispatchUnary/Stream/Mcp route it
  // into the Cursor result's isError / non-zero exit shapes) instead of being reported as a clean success.
  resolvePending(id, content, isError = false, images = null) {
    const p = this.pending.get(id);
    if (!p) return false;
    // ADD-95 (bridge backstop): cap an oversized live tool-result string before it resolves into the resuming
    // run (the executor cap normally wins; this only trims content that bypassed it). Same 'truncated by proxy'
    // marker on both halves. Structured object content is passed through untouched.
    const capped = truncateLiveToolResult(content);
    // EX3: forward any tool-result images to the resolve wrap (3rd arg) so the /mcp tools/call response can
    // carry them as MCP image content when COMPOSER_MCP_IMAGE_RESULTS is on. Wraps that ignore it are unaffected.
    if (p.timer) clearTimeout(p.timer); this.pending.delete(id); p.resolve(capped, isError === true, images);
    // ADD-60: a streaming client can answer a tool BEFORE the TOOL_BATCH_MS debounce flushes (the concurrent
    // activeRes path resolves it into the live run). If that id is still sitting in turnBatch, the pending
    // flush would later emit a STALE `turn_end{tool_use, tool_calls:[id]}` for an already-answered call (the
    // client then sees a stale terminal or re-executes the tool). Drop the resolved id from turnBatch; if the
    // batch empties, cancel the flush timer so no stale tool_use turn fires for satisfied calls.
    if (this.turnBatch.length) {
      this.turnBatch = this.turnBatch.filter((b) => b.id !== id);
      if (this.turnBatch.length === 0 && this.flushTimer) { clearTimeout(this.flushTimer); this.flushTimer = null; }
    }
    return true;
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
      if (this.resolvePending(tr.toolCallId, tr.content, isErr, tr.images)) { matched++; continue; }
      // Explicit idless/minted result: a translator proved there was no client-visible id. Resolve the lone
      // pending ONLY when exactly one is outstanding (never guess among several). This is the sole survivor of
      // the removed C03 fallback and fires only behind the explicit flag.
      if (tr.idless === true && this.pending.size === 1) {
        const loneId = this.pending.keys().next().value;
        dbg("matchToolResults idless 1-pending resolve", "session=" + this.id, "resolving=" + loneId);
        if (this.resolvePending(loneId, tr.content, isErr, tr.images)) { matched++; continue; }
      }
      // BR1: an id that misses but was ever emitted/delivered is benign (reaped or already resolved). Only an
      // id never issued by this session is genuinely unknown.
      if (!this.delivered.has(tr.toolCallId) && !this.everEmitted.has(tr.toolCallId)) unknown.push(tr.toolCallId);
    }
    return { matched, unknown };
  }

  // allToolResultsForeign peeks (NON-mutating) whether EVERY result in the batch carries a concrete id this
  // session never issued — not pending, not delivered, not ever-emitted — and no idless-lone-pending escape
  // applies. That is exactly the set matchToolResults would push entirely into `unknown` while resolving
  // nothing: the orphan / mis-route signal. It happens when the run that OWNED these tool calls died and the Go
  // executor cleared its tool_call ownership (composerApplyLeaseStop -> forgetSession on an error stop), so the
  // tool result got routed by lineage to a DIFFERENT session that cannot own it. Used to return HTTP 410 BEFORE
  // the SSE headers are written — the same lost-continuation signal as an unknown session — so the executor's
  // reseed-on-410 replays the conversation as a fresh user turn instead of emitting an "unknown tool_call_id"
  // error over HTTP 200 that the client then retries forever (the result has no live owner anywhere).
  allToolResultsForeign(results) {
    const batch = results || [];
    if (batch.length === 0) return false;
    for (const tr of batch) {
      if (!tr) return false;
      if (tr.idless === true && this.pending.size === 1) return false; // would resolve the lone pending
      const id = tr.toolCallId;
      if (this.pending.has(id) || this.delivered.has(id) || this.everEmitted.has(id)) return false;
    }
    return true;
  }

  dispatchUnary(cas, spec, s) {
    const id = this.wireToolId(s); // H23: collision-safe per-session wire id
    // ADD-57: resolve the result context (real cwd) at emit time from the session's client env.
    const ctx = { cwd: composerWorkspaceCwd(this.clientEnv) };
    return new Promise((resolve, reject) => {
      // C5/BR8: buildResult sees isError so a native tool the client marked failed (e.g. shell) is routed
      // through the failure shape (non-zero exitCode) instead of being reported to the model as success.
      const wrap = (content, isError) => { try { resolve({ __ccJson: spec.buildResult(content, s, isError === true, ctx) }); } catch (err) { reject(err); } };
      wrap.__reject = reject;
      this.newPending(id, wrap);
      this.emitToolUse(id, spec.ccTool, ccArgsFor(cas, s));
    });
  }
  dispatchStream(cas, spec, s) {
    const id = this.wireToolId(s); // H23: collision-safe per-session wire id
    const self = this;
    const ctx = { cwd: composerWorkspaceCwd(this.clientEnv) }; // ADD-57: real cwd for exit.cwd
    return (async function* () {
      // C5/BR8: carry isError alongside the streamed content so buildChunks can emit a non-zero exit chunk.
      const { content, isError } = await new Promise((resolve, reject) => {
        const wrap = (c, e) => resolve({ content: c, isError: e === true }); wrap.__reject = reject;
        self.newPending(id, wrap);
        self.emitToolUse(id, spec.ccTool, ccArgsFor(cas, s));
      });
      for (const chunk of spec.buildChunks(content, isError, ctx)) yield { __ccJson: chunk };
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
      // ADD-40: list the TURN-SCOPED effective tools (not the restored full set) so a none/specific run reports
      // the correct available surface and never routes a disallowed tool.
      const eff = this.advertiseForGating();
      const names = eff.map((t) => t.toolName || t.name).join(", ");
      dbg("dispatchMcp TOOL NOT AVAILABLE", "want=" + want, "advertisedCount=" + eff.length, "advertised=" + names);
      return Promise.resolve({ __ccJson: mcpDispatchResult(`Tool '${want}' is not available. Available tools: ${names || "(none)"}.`, true) });
    }
    return new Promise((resolve, reject) => {
      // C5/BR4: a client tool that failed/was cancelled (isError) must reach the model AS a failure, so the
      // McpResult's isError mirrors the threaded flag rather than being hardcoded false.
      const wrap = (content, isError, images) => {
        // EX3 (clean path A): this is the LIVE client-tool path (the SDK runtime execs tools in-process via the
        // patched bundle, NOT the HTTP /mcp server). When enabled, fold a tool-result image into the proto
        // McpToolResult as McpImageContent so the model sees it on RESUME — no fresh-send side-channel.
        const imgs = mcpImageResultsEnabled() && Array.isArray(images) && images.length ? images : null;
        if (imgs) dbg("EX3 dispatchMcp folding image into McpToolResult (path A)", "session=" + this.id, "name=" + ccName, "images=" + imgs.length);
        let out = ccName === "Workflow" ? augmentWorkflowResultOnFailure(content, isError) : content;
        out = augmentBackgroundLaunchResult(out, ccName); // a background launch (Workflow, or a backgrounded Bash) -> tell the model to WAIT, not relaunch or redo the work (named + id so it is clear WHICH)
        resolve({ __ccJson: mcpDispatchResult(out, isError, imgs) });
      };
      wrap.__reject = reject;
      this.newPending(id, wrap);
      this.emitToolUse(id, ccName, input);
    });
  }
  // ADD-40: the effective advertised set the MODEL's tool calls are gated against. While a run is live this is
  // the TURN-SCOPED `activeAdvertise` (already restricted by tool_choice none/specific); outside a run it is
  // the full `advertise`. The model only dispatches tools DURING a run, so a tool_choice-disallowed tool can
  // never be reconciled/dispatched from the restored full set anymore.
  advertiseForGating() { return this.activeAdvertise != null ? this.activeAdvertise : (this.advertise || []); }
  reconcileToolName(want) {
    const adv = this.advertiseForGating();
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
    // ADD-104: normalize a non-object tool input to {input:<raw>} ONCE, here, before it is buffered (turnBatch /
    // undelivered) or written to SSE — so the direct emit AND any later flushUndelivered re-emit carry the same
    // stable object shape, and the executor never collapses a string/number/array arg to {}.
    input = wrapToolInput(input);
    // GENERAL MCP-ARG FALLBACK: composer-2.5 sometimes sends a STRING-typed argument as a wrapper OBJECT (e.g.
    // Bash.command / Workflow.script arrived as {…} -> "[object Object]" / a failed command). Coerce each arg the
    // tool's schema types as a PRIMITIVE back to its scalar (pull the string out of a content-block/wrapper),
    // leaving genuinely object/array-typed args untouched. Schema-driven, so it never corrupts a real object arg.
    const advTool = (this.advertise || []).find((t) => (t.toolName || t.name) === name);
    if (advTool && advTool.inputSchema) input = normalizeToolArgsToSchema(name, input, advTool.inputSchema);
    // Workflow value-snap: rewrite a known-wrong agentType/subagent_type VALUE in the script (e.g. generalPurpose
    // -> general-purpose) so the workflow does not fail with "agent type '...' not found".
    if (name === "Workflow" && input && typeof input.script === "string") {
      const snapped = snapWorkflowAgentTypes(input.script);
      if (snapped !== input.script) { input = { ...input, script: snapped }; dbg("emitToolUse: snapped workflow agentType value(s)", "session=" + this.id); }
    }
    if (!this.activeRes || this.writeFailed) {
      // No open client-facing response (the prior turn already closed mid-burst), or the write path is dead.
      // Writing the tool_call to a dead/absent socket would silently create a pending the client can never
      // answer (the desync). Buffer it and deliver it on the next /agent/turn (Comment 1). everEmitted records
      // the issued id (lifetime) so a late result for it is benign, not an "unknown" foreign id.
      dbg("emitToolUse BUFFERED (no activeRes / write dead)", "session=" + this.id, "id=" + id, "name=" + name);
      this.everEmitted.add(id);
      this.undelivered.push({ id, name, input });
      return;
    }
    dbg("emitToolUse", "session=" + this.id, "id=" + id, "name=" + name, "in=" + dbgInputShape(input));
    this.turnBatch.push({ id, name, input });
    // ADD-100: gate delivery bookkeeping on the WRITE actually succeeding. If res.write threw or overflowed the
    // bounded output queue, the client did NOT receive this tool_call — so do NOT mark it delivered/everEmitted
    // and do NOT arm the debounce/pause. Re-buffer it into `undelivered` so it is re-attempted on a later turn,
    // and bail. A client must never have its run paused on a tool it never saw (RBT-009).
    const ok = this.sse({ type: "tool_call", id, name, input });
    if (!ok) {
      dbg("emitToolUse write FAILED -> re-buffer, do not mark delivered/pause (ADD-100)", "session=" + this.id, "id=" + id);
      this.turnBatch = this.turnBatch.filter((b) => b.id !== id);
      this.everEmitted.add(id);
      this.undelivered.push({ id, name, input });
      return;
    }
    // ADD-60: mark the id delivered the moment its tool_call frame is WRITTEN to the client (not only later at
    // pauseForTools). A streaming client may post a continuation answering this id BEFORE the debounce fires;
    // resolvePending then removes it from turnBatch, but the client has genuinely SEEN the id, so an early
    // result for it must be treated as a real match (delivered), never as an unknown/foreign id.
    this.everEmitted.add(id); // BR1: record EVERY issued id (lifetime), so a late result is benign not "unknown"
    this.delivered.add(id);
    const token = this.turnToken;
    if (this.flushTimer) clearTimeout(this.flushTimer);
    this.flushTimer = setTimeout(() => { if (token === this.turnToken) this.maybePauseForTools(token); }, TOOL_BATCH_MS);
  }
  // bufferDelta (#14) stores a text/reasoning delta the run produced while no live response was open (it outlived
  // the turn), preserving order, so it is flushed on the next turn instead of dropped. Bounded: past the cap we
  // stop buffering and log — a stuck/gone client must not grow this unboundedly (the terminal clears it anyway).
  bufferDelta(type, delta) {
    if (this.pendingDeltaBytes + delta.length > COMPOSER_OUT_QUEUE_MAX_BYTES) {
      dbg("bufferDelta OVERFLOW -> drop (no open response, cap reached)", "session=" + this.id, "type=" + type);
      return;
    }
    this.pendingDeltas.push({ type, delta });
    this.pendingDeltaBytes += delta.length;
  }
  // flushPendingDeltas (#14) writes buffered text/reasoning catch-up IN ORDER to the freshly-opened response, so a
  // resuming turn delivers everything the run produced between turns BEFORE its own new output. Re-buffers the
  // remainder on a write failure (never drops). Called at turn-open, before any new output.
  flushPendingDeltas() {
    if (!this.pendingDeltas.length || !this.activeRes || this.writeFailed) return;
    const batch = this.pendingDeltas;
    this.pendingDeltas = []; this.pendingDeltaBytes = 0;
    dbg("flushPendingDeltas", "session=" + this.id, "count=" + batch.length);
    for (let i = 0; i < batch.length; i++) {
      const d = batch[i];
      if (!this.sse({ type: d.type, delta: d.delta })) {
        // write failed mid-flush: re-buffer the remainder (this one + the rest) so nothing is lost.
        this.pendingDeltas = batch.slice(i);
        for (const r of this.pendingDeltas) this.pendingDeltaBytes += r.delta.length;
        break;
      }
    }
  }
  // flushUndelivered delivers tools that were emitted while no response was open, on a later turn's OPEN
  // response, so the client finally sees them and can answer them. Emits one tool_use turn_end + settles.
  flushUndelivered() {
    if (!this.undelivered.length || !this.activeRes || this.writeFailed) return false;
    const batch = this.undelivered;
    this.undelivered = [];
    dbg("flushUndelivered", "session=" + this.id, "count=" + batch.length, "ids=" + safeJson(batch.map((t) => t.id)));
    const delivered = [];
    for (const t of batch) {
      // ADD-100: only mark a tool delivered after its frame WRITES successfully. On a write failure, re-buffer
      // the remainder (this one + the rest) so nothing is marked seen that the client never received.
      const ok = this.sse({ type: "tool_call", id: t.id, name: t.name, input: t.input });
      if (!ok) {
        dbg("flushUndelivered write FAILED -> re-buffer remainder (ADD-100)", "session=" + this.id, "id=" + t.id);
        this.undelivered.unshift(t, ...batch.slice(batch.indexOf(t) + 1));
        break;
      }
      this.delivered.add(t.id); this.everEmitted.add(t.id);
      // ADD-76: the watchdog clock for a buffered tool starts ONLY now, at delivery (it was created without a
      // timer in newPending), so the client gets a fair PENDING_TIMEOUT_MS window from when it first saw the tool.
      this.startPendingTimer(t.id);
      delivered.push(t.id);
    }
    if (!delivered.length) return false; // first write failed before anything was delivered (turn cancelled)
    this.sse({ type: "turn_end", stop_reason: "tool_use", tool_calls: delivered });
    this.settle();
    return true;
  }
  // maybePauseForTools (elegant step-boundary refinement): when the SDK has announced MORE tools for this step
  // (stepToolStarted) than we have delivered (turnBatch), wait one more debounce window for the rest of the
  // wave instead of pausing now — so a slow burst lands in ONE turn_end rather than spilling its tail into the
  // next turn's buffer. BOUNDED by TOOL_BATCH_MAX_EXTENSIONS so an over-count (an announced tool that never
  // dispatches) can never hang the turn. With no step signal (stepToolStarted==0, e.g. unit tests) it pauses
  // immediately — identical to the old debounce. It only ever DELAYS the pause, so it can never strand a tool.
  maybePauseForTools(token) {
    if (this.stepToolStarted > this.turnBatch.length && this.batchWaitExtensions < TOOL_BATCH_MAX_EXTENSIONS) {
      this.batchWaitExtensions++;
      dbg("maybePauseForTools: awaiting full step batch", "session=" + this.id,
        "delivered=" + this.turnBatch.length, "announced=" + this.stepToolStarted, "ext=" + this.batchWaitExtensions);
      this.flushTimer = setTimeout(() => { if (token === this.turnToken) this.maybePauseForTools(token); }, TOOL_BATCH_MS);
      return;
    }
    this.batchWaitExtensions = 0;
    this.pauseForTools();
  }
  pauseForTools() {
    this.flushTimer = null;
    // ADD-106 (Comment 3): COUNT this tool-result round and enforce the per-logical-run loop bounds BEFORE
    // delivering the batch. If a bound trips, tear the run down as a MODEL/CLIENT-visible error terminal instead
    // of pausing for tools — so a runaway agentic loop (endless tool rounds, or the same tool+args hammered) can
    // never stream/pause forever, and is NEVER reported as a clean success.
    if (this.checkLoopBound()) return;
    // ADD-76: arm the abandonment watchdog for each batched id NOW, at the moment it is delivered in the
    // tool_use turn_end (the pending was created without a timer in newPending). ADD-100: the ids are already in
    // `delivered` (set at successful emit time); the turn_end write is best-effort here (the run pauses either
    // way and the ids are tracked), so a failed terminal write does not strand bookkeeping.
    for (const b of this.turnBatch) { this.delivered.add(b.id); this.startPendingTimer(b.id); }
    this.sse({ type: "turn_end", stop_reason: "tool_use", tool_calls: this.turnBatch.map((b) => b.id) });
    this.turnBatch = [];
    this.stepToolStarted = 0; this.batchWaitExtensions = 0; // the step's batch is delivered; the next step re-counts
    this.settle();
  }
  settle() { const f = this.settleTurn; this.settleTurn = null; if (this.flushTimer) { clearTimeout(this.flushTimer); this.flushTimer = null; } if (f) f(); }

  // resetLoopBounds clears the per-logical-run agentic-loop counters (ADD-106). Called when a FRESH send starts a
  // new logical run (driveUserSend) — NOT on a tool_results resume, which continues the SAME logical run and so
  // keeps accumulating rounds. A cancel/supersession also resets them via the next fresh send.
  resetLoopBounds() { this.toolRounds = 0; this.lastToolSig = null; this.repeatToolCount = 0; this.loopTripped = false; this.stashedToolResultImages = []; }

  // checkLoopBound (ADD-106) counts ONE tool-result round (the batch about to be delivered in pauseForTools) and
  // enforces the per-logical-run bounds. Returns true when a bound TRIPPED (the run was terminated as an error
  // terminal and the caller must NOT proceed to pause for tools); false otherwise. A single-tool round whose
  // (name+args) signature repeats consecutively advances the repeat counter; any other shape resets it. The
  // bounds are COUNTS (not timers), so this adds no wall-clock deadline to the established data path.
  checkLoopBound() {
    if (this.loopTripped) return true; // already terminated this run — never pause again
    this.toolRounds++;
    // Repeat detector: only a SINGLE-tool round has a well-defined "same call" signature. A multi-tool round
    // (parallel batch) is genuine progress, so it resets the consecutive-repeat streak.
    const batch = this.turnBatch;
    if (batch.length === 1) {
      const sig = batch[0].name + " " + safeJson(batch[0].input);
      if (sig === this.lastToolSig) this.repeatToolCount++;
      else { this.lastToolSig = sig; this.repeatToolCount = 1; }
    } else {
      this.lastToolSig = null; this.repeatToolCount = 0;
    }
    if (this.toolRounds > COMPOSER_MAX_TOOL_ROUNDS) {
      this.tripLoopBound(`composer run exceeded the tool-round bound (${COMPOSER_MAX_TOOL_ROUNDS}); aborting a likely runaway agentic loop`);
      return true;
    }
    if (COMPOSER_MAX_REPEAT_TOOL > 0 && this.repeatToolCount >= COMPOSER_MAX_REPEAT_TOOL) {
      const name = batch.length === 1 ? batch[0].name : "(tool)";
      this.tripLoopBound(`composer run repeated the same tool call (${name}) ${this.repeatToolCount} times consecutively; aborting a likely stuck loop`);
      return true;
    }
    return false;
  }

  // tripLoopBound (ADD-106) terminates the current logical run because a loop bound was exceeded. It mirrors the
  // onRunError teardown (error terminal + reject pendings + clear state + settle + notifyLogicalDone so the FIFO
  // advances) but is driven by a COUNT bound, not an upstream error. The terminal is turn_end{stop_reason:"error"}
  // — a typed loop-bound error the model/client sees — NEVER a clean end_turn/[DONE] (a false success). It also
  // cancels the live SDK run so the upstream stops producing, and bumps runEpoch (via cancel) so the superseded
  // run's late wait()/stream callbacks cannot leak into a successor turn.
  tripLoopBound(reason) {
    if (this.loopTripped) return;
    this.loopTripped = true;
    this.lastRunError = reason; // BR2: a later tool_results that finds the run gone surfaces this real error
    dbg("tripLoopBound", "session=" + this.id, "rounds=" + this.toolRounds, "repeat=" + this.repeatToolCount, reason);
    this.sse({ type: "turn_end", stop_reason: "error", error: reason });
    this.settle();
    // cancel() tears down the live run/agent, rejects every pending, bumps runEpoch (epoch-gating the dead run's
    // callbacks), and fires notifyLogicalDone so a queued new-user turn is admitted. done=true short-circuits any
    // late onRunComplete/onRunError from the cancelled run so the error terminal above is the run's only terminal.
    void this.cancel();
  }

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
    const fullResult = (res && res.result) || "";
    if (!this.streamedText && fullResult) this.sse({ type: "text", delta: fullResult });
    // ERROR HONESTY (Comment 3): a run that "finished" but produced NO user-visible output this run — no streamed
    // text, no result lump, and no tool call delivered OR buffered — is an EMPTY turn, not a success. Surface it
    // as turn_end{error} so the client is never handed a clean empty completion (a false success). `delivered`
    // and `undelivered` accumulate across the run's tool rounds (clearTurnState runs only at the terminal), so
    // this is accurate for a multi-round run and only fires on a genuinely empty finished run.
    const finished = res && res.status === "finished";
    // #15: reasoning, still-buffered catch-up deltas, and any reported usage ALSO count as produced output, so a
    // run that emitted only reasoning (or whose text is buffered awaiting flush) is not mis-flagged as an empty
    // turn (a false error). The empty->error path fires ONLY on a finished run with genuinely nothing produced.
    const hasUsage = !!(res && res.usage && typeof res.usage === "object" && Object.keys(res.usage).length > 0);
    const producedOutput = !!this.streamedText || this.reasonedThisRun || !!fullResult
      || this.delivered.size > 0 || this.undelivered.length > 0 || this.pendingDeltas.length > 0 || hasUsage;
    const stopReason = finished && producedOutput ? "end_turn" : "error";
    // A successful run proves the upstream connection is healthy -> close any rate-limit breaker for this key.
    if (stopReason === "end_turn") closeBreaker(keyHash(this.cursorKey));
    const turnError = finished
      ? (producedOutput ? (res && res.error) : "composer run finished with no output (empty turn)")
      : ((res && res.error) || "composer run did not finish");
    this.sse({ type: "turn_end", stop_reason: stopReason, status: res && res.status, error: turnError, usage: (res && res.usage) || {} });
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
  // composeAgentId derives the durable agentId from the external id plus BOTH rotation epochs (C05 too-long
  // rotation + ADD-62 model-change rotation). Epoch 0/0 keeps the id == external id (the original behavior, so
  // existing durable state is unchanged); any rotation appends a stable, unique suffix (`_rN` and/or `_mN`).
  // Combining both epochs guarantees a fresh unique id even when a model change follows a too-long rotation.
  composeAgentId() {
    let aid = this.id;
    if (this.recoveryEpoch > 0) aid += `_r${this.recoveryEpoch + 1}`; // first too-long rotation -> _r2
    if (this.modelEpoch > 0) aid += `_m${this.modelEpoch}`;           // first model change   -> _m1
    if (this.keyEpoch > 0) aid += `_k${this.keyEpoch}`;               // ADD-79: first key change -> _k1
    return aid;
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
    this.agentId = this.composeAgentId(); // first rotation -> <id>_r2 (+ _mN if a model change also happened)
    this.resetSeedState();
    dbg("rotateDurableAgent CONVERSATION_TOO_LONG -> rotate durable agentId (no resumeAgent(old))", "session=" + this.id, "old=" + oldAgentId, "new=" + this.agentId);
    // Tear down the poisoned live agent/run WITHOUT deleting the session. cancel() nulls agent/run + rejects
    // pendings; we re-open the session for the next turn afterwards (done=false) so it can re-seed.
    await this.finishRotationCancel();
  }
  // rotateForModelChange (ADD-62) rotates the durable agent when the conversation switches model. The old
  // durable agent is bound to the OLD model (resumeAgent would silently keep answering from it), so we
  // tombstone it, allocate a fresh agentId under a SEPARATE modelEpoch budget (so model toggling never burns
  // the C05 crash-recovery rotations), and force a re-seed of the client's history into the fresh agent under
  // the new model. The external session id is KEPT so continuations keep routing here. Per owner decision:
  // rotate + re-seed (NOT 409-reject, NOT silently reuse the old model's agent). Bounded so pathological
  // model-flapping cannot churn agentIds without limit; past the cap we keep the last rotated agent (the next
  // turn re-seeds into it) rather than dropping the session.
  async rotateForModelChange(newModel) {
    const COMPOSER_MAX_MODEL_ROTATIONS = 8;
    const oldAgentId = this.agentId;
    const oldModel = this.model;
    if (this.modelEpoch < COMPOSER_MAX_MODEL_ROTATIONS) {
      this.modelEpoch++;
      this.agentId = this.composeAgentId(); // first model change -> <id>_m1 (+ _rN if a too-long rotation also happened)
    } else {
      dbg("rotateForModelChange cap reached -> keep last agentId, force re-seed", "session=" + this.id, "agentId=" + this.agentId, "epoch=" + this.modelEpoch);
    }
    this.model = newModel;
    this.resetSeedState();
    dbg("rotateForModelChange -> rotate durable agentId for new model (no resumeAgent(old model's agent))", "session=" + this.id, "old=" + oldAgentId, "new=" + this.agentId, "oldModel=" + oldModel, "newModel=" + newModel);
    await this.finishRotationCancel();
  }
  // rotateForKeyChange (ADD-79) rotates the durable agent when the upstream Cursor key changes for the SAME
  // external session (a tenant rotates their key, an admin rebinds it, or multi-tenant forwards a different
  // per-user key under the same conversation id). The old durable agent lives under the OLD key's account +
  // stateRoot; resumeAgent would silently keep answering from it (stale/revoked creds, wrong billing, wrong
  // isolation). So we tombstone it, point session.cursorKey at the NEW key, allocate a fresh agentId under a
  // SEPARATE keyEpoch budget (so a key rotation never burns the C05 crash-recovery or ADD-62 model rotations),
  // and force a re-seed of the client's history into the fresh agent on the NEW key's platform. The external
  // session id is KEPT so continuations keep routing here. Bounded so pathological key-flapping cannot churn
  // agentIds without limit; past the cap we keep the last rotated agent (the next turn re-seeds into it). Per the
  // CURSOR-KEY-FINGERPRINT contract: NEVER mutate session.cursorKey on a live run without rotating the durable
  // agent — this method does both atomically (set the key, rotate the id, then cancel the old-key run).
  async rotateForKeyChange(newKey) {
    const COMPOSER_MAX_KEY_ROTATIONS = 8;
    const oldAgentId = this.agentId;
    if (this.keyEpoch < COMPOSER_MAX_KEY_ROTATIONS) {
      this.keyEpoch++;
      this.agentId = this.composeAgentId(); // first key change -> <id>_k1 (+ _rN/_mN if those also happened)
    } else {
      dbg("rotateForKeyChange cap reached -> keep last agentId, force re-seed", "session=" + this.id, "agentId=" + this.agentId, "epoch=" + this.keyEpoch);
    }
    this.cursorKey = newKey || API_KEY; // run subsequent turns on the NEW key's platform/account
    this.resetSeedState();
    dbg("rotateForKeyChange -> rotate durable agentId for new key (no resumeAgent(old key's agent))", "session=" + this.id, "old=" + oldAgentId, "new=" + this.agentId);
    await this.finishRotationCancel();
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
    this.stepToolStarted = 0; this.batchWaitExtensions = 0; // #85: reset the step batch counters at the terminal
    this.pendingDeltas = []; this.pendingDeltaBytes = 0; // #14: drop any undelivered text/reasoning; the run is over
    this.activeAdvertise = null; // ADD-40: the turn-scoped effective tool policy ends with the run
    // ADD-100: drop any queued backpressured output + detach the 'drain' listener; the run is over.
    this.outQueue = []; this.outQueueBytes = 0; this.detachDrain();
  }
  // cancel tears down the live run/agent + rejects pendings. ADD-90: `notify` controls whether queued waiters
  // are released (notifyLogicalDone). External callers (onClose, handleTurn interrupt, rotate*, shutdown, evict,
  // failWrite) use the default notify:true so the FIFO advances. Internal SUPERSESSION (cancelStaleRun) passes
  // notify:false so a queued new-user turn is NOT promoted before driveUserSend installs the replacement
  // session.run — otherwise the queued turn and the replacement send would race on the same durable agent.
  async cancel({ notify = true } = {}) {
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
    if (notify) this.notifyLogicalDone(); // run torn down -> release any queued waiter so the chain advances
  }
}

function nowMs() { return Date.now(); }

// ADD-49: a bounded FIFO id-set used for `everEmitted` (the per-session lifetime record of issued tool ids).
// A long-lived session emitting thousands of tool calls across many runs would otherwise let `everEmitted`
// grow without bound (memory) AND keep treating ancient stale ids as "benign" forever. We bound it: only the
// most recent EVER_EMITTED_CAP ids are retained; an older id falls out and, if a stale result for it arrives,
// it is treated as genuinely unknown (a real desync) rather than a permanent benign downgrade. The cap is
// generous so a normal multi-run conversation never evicts a still-relevant id. has()/add() are O(1).
const EVER_EMITTED_CAP = 4096;
class BoundedIdSet {
  constructor(cap = EVER_EMITTED_CAP) { this.cap = cap; this.set = new Set(); }
  add(id) {
    if (this.set.has(id)) { this.set.delete(id); this.set.add(id); return; } // refresh recency (true LRU touch)
    this.set.add(id);
    if (this.set.size > this.cap) { const oldest = this.set.values().next().value; this.set.delete(oldest); }
  }
  has(id) { return this.set.has(id); }
  get size() { return this.set.size; }
  clear() { this.set.clear(); }
  values() { return this.set.values(); }
  [Symbol.iterator]() { return this.set[Symbol.iterator](); }
}

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

// composerModelSelection maps an incoming model id to a Cursor SDK ModelSelection ({ id, params }) using this
// fork's GPT-style dash-suffix convention (e.g. gpt-5.2-xhigh). Cursor's DEFAULT composer-2.5/composer-2 variant
// is fast=true — the more EXPENSIVE fast tier (confirmed via Cursor.models.list(): a `fast` boolean param whose
// fast=true variant is isDefault), so a bare { id } silently selects fast. Mapping:
//   composer-2.5         -> { fast:false }                    (the full tier; omit suffix => our default)
//   composer-2.5-fast    -> { fast:true }                     (the fast variant, an available model)
//   composer-2.5-<level> -> { fast:false, thinking:<level> }  (reasoning effort; per the Cursor docs composer
//                                                              accepts `thinking`, values vary by account)
// Non-composer / unrecognized ids pass through unchanged (Cursor resolves their own default). The level value is
// passed THROUGH (Cursor validates) so the per-account level set never has to be hardcoded.
const COMPOSER_THINKING_LEVELS = new Set(["minimal", "none", "low", "medium", "high", "xhigh", "max"]);
function composerModelSelection(model) {
  const raw = String(model || "");
  let id = raw;
  let fast = "false";
  let thinking = null;
  // Suffix order is base[-fast][-<level>]: strip the innermost reasoning level first, then the -fast variant, so
  // composer-2.5-fast-high -> { fast:true, thinking:high }, composer-2.5-high -> { fast:false, thinking:high }.
  const d = id.lastIndexOf("-");
  if (d > 0 && COMPOSER_THINKING_LEVELS.has(id.slice(d + 1).toLowerCase())) {
    thinking = id.slice(d + 1).toLowerCase();
    id = id.slice(0, d);
  }
  if (/-fast$/.test(id)) { fast = "true"; id = id.slice(0, id.length - "-fast".length); }
  if (id === "composer-2.5" || id === "composer-2") {
    const params = [{ id: "fast", value: fast }];
    if (thinking) params.push({ id: "thinking", value: thinking });
    return { id, params };
  }
  return { id: raw }; // non-composer / unrecognized: pass the original id through (Cursor resolves its default)
}

async function ensureAgent(session, model) {
  if (session.agent) return session.agent;
  if (session.agentPromise) return session.agentPromise;          // guard TOCTOU
  session.agentPromise = (async () => {
    const platform = await getPlatform(session.cursorKey);
    const modelSel = composerModelSelection(model);
    dbg("ensureAgent modelSelection", "session=" + session.id, "model=" + model, "selection=" + safeJson(modelSel));
    const opts = { model: modelSel, apiKey: session.cursorKey, local: { cwd: EMPTY_CWD } };
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
      // BR-DS / H11 / H12 / ADD-73: a SUCCESSFUL resume means this durable agentId EXISTS in the SDK store —
      // which only happens after a prior createAgent + at least one send (the seed); the create-on-not-found
      // branch below is the ONLY path that mints a fresh, unseeded agent. So a successful resume of an unseeded
      // in-memory session is a COLD RESTART of a previously-seeded durable agent: the SDK already holds the
      // conversation, and re-prepending the entire client history on top would DOUBLE the context and risk
      // ERROR_CONVERSATION_TOO_LONG. We therefore mark seeded=true on a successful resume, with ONE exception:
      // if the message probe is available AND explicitly returns EMPTY, the durable agent genuinely has no
      // turns -> leave unseeded so the next send seeds it.
      //   - probe returns non-empty -> seeded (has prior turns).
      //   - probe returns EMPTY     -> NOT seeded (truly empty agent; seed it).
      //   - probe THROWS or is absent (ADD-73) -> seeded (a resumed durable agent almost certainly has turns;
      //     guessing "unseeded" on a failed probe was the double-seed bug — never silently double-seed).
      // `reseeding` (a /compact, incl. H12 cold-restart compact) is honored: runTurn WANTS to re-prepend the
      // rewritten history, so we never mark seeded then.
      if (!session.seeded && !session.reseeding) {
        let markSeeded = true; // default for a successful resume (ADD-73): assume the durable agent has state
        if (typeof platform.getAgentMessages === "function") {
          try {
            const prior = await platform.getAgentMessages(agentId, { limit: 1 });
            // An explicit EMPTY result is the only signal that the durable agent has no turns -> seed it.
            if (Array.isArray(prior) && prior.length === 0) {
              markSeeded = false;
              dbg("ensureAgent resume probe returned EMPTY -> leave unseeded (seed on next send)", "session=" + session.id);
            } else {
              dbg("ensureAgent resume found prior turns -> seeded=true (no re-prepend)", "session=" + session.id, "priorCount>=" + (Array.isArray(prior) ? prior.length : "?"));
            }
          } catch (probeErr) {
            // ADD-73: the probe is the only completeness signal and it FAILED. Do NOT guess "unseeded" (that
            // re-prepends history into an agent that already has it). A successful resume implies prior state,
            // so mark seeded — the durable fingerprint (H12) still catches a genuine compact-across-restart.
            dbg("ensureAgent getAgentMessages probe THREW -> seeded=true on successful resume (avoid double-seed, ADD-73)", "session=" + session.id, (probeErr && probeErr.message) || String(probeErr));
          }
        } else {
          dbg("ensureAgent resume (no message probe) -> seeded=true on successful resume (avoid double-seed, ADD-73)", "session=" + session.id);
        }
        if (markSeeded) session.seeded = true;
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
  // ADD-40: tools/list during a live run reflects the turn-scoped effective set (tool_choice-gated), so a
  // none/specific run does not even ADVERTISE a disallowed tool through the shim.
  const adv = (session && session.advertiseForGating && session.advertiseForGating()) || (session && session.advertise) || [];
  const all = grouping === "one" || !serverKey || serverKey === "cc";
  const out = [];
  for (const t of adv) {
    const name = t.toolName || t.name;
    if (!name) continue;
    if (!all && mcpServerKeyForTool(name, grouping) !== serverKey) continue;
    const schema = t.inputSchema && typeof t.inputSchema === "object" ? t.inputSchema : { type: "object" };
    // Inject a schema-derived argument contract (+ any per-tool extra) so the model calls each tool with its exact
    // arg shape and never conflates tools. ONLY in what composer reads here; session.advertise stays untouched.
    out.push({ name, description: augmentToolDescription(name, t.description || "", schema), inputSchema: schema });
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
    const sid = session.id;
    const mkServer = (serverKey) => ({
      type: "http",
      url: `http://127.0.0.1:${PORT}/mcp/${sid}` + (serverKey && serverKey !== "cc" ? `/${serverKey}` : ""),
      headers: { "X-CC-Session": sid },
    });
    // Comment 6: register at least ONE session-scoped loopback server even when the CURRENT turn advertises NO
    // tools. The durable agent's mcpServers map is fixed when the agent is first created/resumed; if we returned
    // {} on a tool-less first turn, the SDK would never dial /mcp and a tool advertised on a LATER turn could not
    // surface (tools/list reads session.advertise DYNAMICALLY, so the empty server still picks up later tools
    // without rotating the durable agent). Always-register is the simpler path (no advertise-transition rotation).
    if (!adv.length) { dbg("buildMcpServers: no tools this turn -> register one empty session server (Comment 6)", "session=" + sid); return { cc: mkServer("cc") }; }
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

// headlessMcpState answers the SDK runtime's mcp_state_exec CLIENT request — the headless equivalent of the
// Cursor IDE reporting its MCP inventory. The runtime feeds the model its MCP toolset from THIS reply
// (mcpStateAccessor.getState); answering it with a typed-unavailable error (the old TYPED_UNAVAILABLE_U
// behavior) made the backend expose ZERO MCP tools even though the loopback servers were dialed + tools/list'd,
// so composer never invoked an advertised MCP tool (observed dispatchMcp=0, no tools/call). We report each
// DIALED loopback server (buildMcpServers is the authoritative set — same keys, INCL. the Comment-6
// always-register-"cc" and the natural collision-degrade) with its currently-enabled tool slice
// (mcpToolsForServer is tool_choice-gated, ADD-40). server_identifier == the dialed server key so the runtime
// correlates a state server to its dialed counterpart; tool name == tool_name and providerIdentifier:"cc"
// match the run-request mcp_tools advertise so the backend treats them as the SAME tool; status:"connected" is
// the runtime's "ready" value (a "needsAuth" server is filtered out). Fail-safe: empty/absent advertise (or
// shim off) -> { servers: [] }, an HONEST "no servers" success (never a fabricated tool, strictly better than
// the old error); any throw falls back to the typed-unavailable error (no worse than before). HANDLER change
// only — the exec was already routed to __CC_EXEC_U by the patch, exactly like requestContextArgs.
function headlessMcpState(session) {
  try {
    const dialed = buildMcpServers(session); // authoritative dialed-server set; keys match what the SDK dialed
    const servers = [];
    for (const key of Object.keys(dialed)) {
      const tools = mcpToolsForServer(session, key).map((t) => ({
        name: t.name,
        toolName: t.name, // dispatchMcp reconciles by name; keep tool_name == name
        providerIdentifier: "cc", // matches the run-request mcp_tools provider so it is the SAME tool
        description: t.description || "",
        inputSchema: t.inputSchema && typeof t.inputSchema === "object" ? t.inputSchema : { type: "object" },
      }));
      servers.push({ serverName: key, serverIdentifier: key, status: "connected", tools });
      dbgToolFormat("mcpState", session.id, key, tools); // verbose: the exact per-tool schema the model receives
    }
    return { __ccJson: { success: { servers } } };
  } catch (e) {
    dbg("headlessMcpState threw -> typed-unavailable fallback", "session=" + (session && session.id), (e && e.message) || String(e));
    return typedUnavailableResult("mcpStateExecArgs");
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
        dbgToolFormat("tools/list", sessionId, serverKey, tools); // verbose: the exact per-tool schema the model receives
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
          const out = await new Promise((resolve, reject) => {
            const wrap = (c, _e, imgs) => resolve({ content: c, images: imgs });
            wrap.__reject = reject;
            session.newPending(callId, wrap);
            session.emitToolUse(callId, ccName, input);
          });
          // EX3 (clean path B): return inline base64 tool-result images as MCP image content, BEFORE the text, so
          // the model sees the image on RESUME. Standard MCP CallToolResult content part {type:"image",data,mimeType}.
          // url-form images are not base64 here, so they fall through to the text + the (flag-off) fresh-send path.
          const parts = [];
          if (mcpImageResultsEnabled() && Array.isArray(out.images)) {
            for (const im of out.images) {
              if (im && typeof im.data === "string" && im.data && typeof im.mimeType === "string" && im.mimeType) {
                parts.push({ type: "image", data: im.data, mimeType: im.mimeType });
              }
            }
            if (parts.length) console.error("[cct] EX3 mcp tools/call (path B) returning image content session=" + sessionId + " name=" + ccName + " images=" + parts.length);
          }
          const text = typeof out.content === "string" ? out.content : JSON.stringify(out.content ?? "");
          if (text || parts.length === 0) parts.push({ type: "text", text });
          return mcpResult(id, { content: parts, isError: false });
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

function streamCallbacks(session, ep) {
  // #13: EPOCH-GATE the producer. A superseded/cancelled run (cancel() bumps runEpoch) or a completed session
  // must never write text/reasoning into a SUCCESSOR turn's activeRes or mutate its buffers — that is the
  // cross-run output leak. ep is captured at agent.send; every callback no-ops once the epoch advances or the
  // session is done. (The completion callback at run.wait() is already epoch-gated the same way.)
  const live = () => ep === session.runEpoch && !session.done;
  return {
    onDelta: ({ update }) => {
      try {
        if (!live()) return; // #13: drop a dead/superseded run's late delta — never leak into a successor turn
        const ty = update && (update.type || update.case);
        const txt = update && (update.text != null ? update.text : (update.value && update.value.text));
        if (ty === "text-delta" && txt) {
          session.streamedText += txt;
          // #14: if no response is open (the run outlived the turn), BUFFER the text in order rather than
          // dropping it. sse() returns false in exactly that case (no activeRes / write dead).
          if (!session.sse({ type: "text", delta: txt })) session.bufferDelta("text", txt);
        } else if (ty === "thinking-delta" && txt) {
          session.reasonedThisRun = true; // #15: reasoning counts as produced output
          if (!session.sse({ type: "reasoning", delta: txt })) session.bufferDelta("reasoning", txt);
        }
        // STEP-BOUNDARY signal (Q1, validated E2E): the SDK announces each tool of a step via `tool-call-started`
        // BEFORE our dispatch seam emits it. Count them per step so the pause can WAIT for a slow batch (see
        // maybePauseForTools) instead of guessing with the debounce alone. step/turn boundaries reset the count.
        // This is a latency refinement layered on the TOOL_BATCH_MS floor — never the sole correctness barrier.
        else if (ty === "tool-call-started" || ty === "partial-tool-call" || ty === "tool-call-completed"
              || ty === "turn-ended" || ty === "step-started" || ty === "step-completed") {
          if (ty === "tool-call-started") session.stepToolStarted++;
          else if (ty === "step-started" || ty === "step-completed" || ty === "turn-ended") { session.stepToolStarted = 0; session.batchWaitExtensions = 0; }
          try { dbg("onDelta STEP", "session=" + session.id, "type=" + ty, "stepToolStarted=" + session.stepToolStarted,
            "turnBatch=" + session.turnBatch.length, "undelivered=" + session.undelivered.length, "activeRes=" + !!session.activeRes); } catch { /* never throw from a probe */ }
        }
        // A NON-EMPTY delta whose discriminator we don't recognize would otherwise be silently dropped (the
        // model's answer vanishes behind a clean turn_end). Surface it so a future @cursor/sdk discriminator
        // rename (text-delta/thinking-delta) is visible in the operational logs instead of failing silently.
        else if (txt && ty !== "text-delta" && ty !== "thinking-delta") dbg("onDelta UNRECOGNIZED non-empty delta type -> dropped (SDK discriminator drift?)", "session=" + session.id, "type=" + safeJson(ty));
      } catch (e) { dbg("onDelta ERROR", "session=" + session.id, (e && e.message) || String(e)); }
    },
    // onStep PROBE: empirically discover the @cursor/sdk step boundary so the deadlock fix can replace the
    // TOOL_BATCH_MS timing heuristic with a REAL step-complete signal (keep activeRes open until the assistant
    // step is truly done -> no tool-use stranded after a too-early debounce pause). Logged, not yet acted on.
    onStep: (step) => {
      if (!live()) return; // #13: ignore a dead run's step callback
      try {
        dbg("onStep PROBE", "session=" + session.id, "turnBatch=" + session.turnBatch.length,
          "undelivered=" + session.undelivered.length, "activeRes=" + !!session.activeRes,
          "shape=" + safeJson(step && typeof step === "object" ? Object.keys(step) : typeof step));
      } catch { /* never throw from a probe */ }
    },
  };
}

// ---- HTTP ----
// dbg writes a GUARANTEED-FLUSHED line to stdout (fd 1) so the sidecar's operational logs reach Railway
// even though Node block-buffers pipe stdout. Lines are content-free (session ids, statuses, lengths,
// error messages) — turn routing decisions and failures only, never request/response bodies.
function safeJson(a) { try { return typeof a === "string" ? a : JSON.stringify(a); } catch { return String(a); } }
function dbg(...args) { if (!COMPOSER_DEBUG) return; try { writeSync(1, "[cct] " + args.map(safeJson).join(" ") + "\n"); } catch { /* never throw from logging */ } }

// dbgToolFormat (debug-only): dumps the EXACT per-tool shape the model receives via tools/list / mcpState —
// name, description length, and inputSchema property/required counts. A tool whose real schema was lost and
// flattened to the bare {type:object} default (mcpToolsForServer/:headlessMcpState fallback) shows "BARE": the
// model cannot construct a valid call for it and will avoid it, yet it still counts as "0 missing schema"
// upstream. This is the only place that surfaces a degraded advertisement (e.g. a Workflow tool with no params).
function dbgToolFormat(tag, sessionId, serverKey, tools) {
  if (!COMPOSER_DEBUG) return;
  try {
    const parts = (tools || []).map((t) => {
      const nm = (t && (t.toolName || t.name)) || "?";
      const s = t && t.inputSchema && typeof t.inputSchema === "object" ? t.inputSchema : null;
      const keys = s && s.properties && typeof s.properties === "object" ? Object.keys(s.properties) : [];
      const props = keys.length;
      const req = s && Array.isArray(s.required) ? s.required.length : 0;
      const bare = !s || (props === 0 && req === 0);
      const keyStr = props > 0 && props <= 16 ? "[" + keys.join(",") + "]" : "";
      return nm + "{d=" + (t && t.description ? String(t.description).length : 0) + " p=" + props + keyStr + " r=" + req + (bare ? " BARE" : "") + "}";
    });
    const bareN = parts.filter((p) => p.indexOf(" BARE}") >= 0).length;
    dbg(tag + " TOOLFMT", "session=" + sessionId, "serverKey=" + (serverKey || "cc"), "n=" + parts.length, "bareSchema=" + bareN, parts.join(" "));
  } catch { /* never throw from a probe */ }
}

// dbgInputShape (debug-only): a compact, content-light summary of a tool-call's arguments — per-key value
// length/type — so we can see whether the model sent the REQUIRED fields (e.g. a Workflow call's `script`) or
// omitted them (calling Workflow with only a title runs an empty workflow). Never logs the raw values.
function dbgInputShape(input) {
  try {
    if (input == null) return "null";
    if (typeof input !== "object") return typeof input + "(" + String(input).length + "ch)";
    return "{" + Object.keys(input).map((k) => {
      const v = input[k];
      if (v == null) return k + "=null";
      if (typeof v === "string") return k + "=" + v.length + "ch";
      if (Array.isArray(v)) return k + "=arr" + v.length;
      if (typeof v === "object") return k + "=obj" + safeJson(v).slice(0, 200); // dump the wrapper so an object-valued arg is visible
      return k + "=" + typeof v;
    }).join(" ") + "}";
  } catch { return "?"; }
}

// extractScalarFromWrapper pulls a primitive (string/number/boolean) out of a wrapper OBJECT the model/SDK may
// emit for a scalar arg — a content block {type:"text",text:…} or {text}/{value}/{content}/{string}/{input}, the
// same-key nesting {command:{command:…}}/{script:{script:…}}, or an object with exactly one own property of the
// wanted type. Returns undefined when nothing matches (the caller leaves the value untouched).
function extractScalarFromWrapper(obj, wantType, sameKey) {
  if (obj == null || typeof obj !== "object" || Array.isArray(obj)) return undefined;
  const ok = (v) => (wantType === "string" ? (typeof v === "string" ? v : undefined)
    : wantType === "number" ? (typeof v === "number" ? v : (typeof v === "string" && v.trim() !== "" && !Number.isNaN(Number(v)) ? Number(v) : undefined))
    : wantType === "boolean" ? (typeof v === "boolean" ? v : (v === "true" ? true : v === "false" ? false : undefined)) : undefined);
  const keys = ["text", "value", "content", "string", "input"];
  if (sameKey) keys.unshift(sameKey);
  for (const k of keys) { const got = ok(obj[k]); if (got !== undefined) return got; }
  const own = Object.keys(obj);
  if (own.length === 1) { const got = ok(obj[own[0]]); if (got !== undefined) return got; }
  // STRING-LIKE OBJECT: composer-2.5 delivers some string args as an object whose JSON serialization is a bare
  // string LITERAL (a boxed String, or a value wrapper whose toJSON returns the string) — NOT a {text}/{value}
  // block, so the key probes above miss it (observed: Bash.command / Workflow.script arrived this way). For a
  // string-typed param, recover the primitive: if JSON.stringify yields a "…" literal, parse it back. Safe — a
  // genuine object serializes to {…}/[…] (never starts with "), so a real object arg is never touched.
  if (wantType === "string") {
    try {
      const j = JSON.stringify(obj);
      if (typeof j === "string" && j.length >= 2 && j.charCodeAt(0) === 34 && j.charCodeAt(j.length - 1) === 34) return JSON.parse(j);
    } catch { /* not serializable -> fall through, leave untouched */ }
  }
  return undefined;
}

// unwrapPrimitiveLikeObject recovers the primitive from an object whose JSON serialization is a BARE primitive
// literal — a boxed String/Number/Boolean, or any value-wrapper whose toJSON returns a primitive. composer-2.5
// delivers many scalar args this way (string args as boxed Strings; booleans as boxed Booleans, e.g. the Agent
// spawn's readonly=new Boolean(true) -> "Invalid tool parameters"). Schema-INDEPENDENT and universally safe: a
// genuine object/array serializes to {…}/[…] (never a bare ", -, digit, t, or f), so a real object arg is never
// touched. Returns undefined for anything that is not primitive-like.
function unwrapPrimitiveLikeObject(v) {
  try {
    if (v == null || typeof v !== "object" || Array.isArray(v)) return undefined;
    const j = JSON.stringify(v);
    if (typeof j !== "string" || !j.length) return undefined;
    const c = j.charCodeAt(0);
    if (c === 34 /* " */ || c === 45 /* - */ || (c >= 48 && c <= 57) /* 0-9 */ || c === 116 /* t */ || c === 102 /* f */) {
      const p = JSON.parse(j);
      if (typeof p === "string" || typeof p === "number" || typeof p === "boolean") return p;
    }
    return undefined;
  } catch { return undefined; }
}

// normalizeToolArgsToSchema is the general MCP-arg fallback (see emitToolUse): composer-2.5 wraps scalar args in
// objects. For EVERY arg: (1) universally unwrap a primitive-like object (boxed String/Number/Boolean) — covers
// any arg, INCLUDING ones not in the schema (e.g. an extra readonly:true); then (2) for an arg the schema types
// as a primitive but is still an object, pull the scalar from a content-block/wrapper ({type:text,text} etc.).
// Best-effort + fail-safe — an arg it cannot coerce is left untouched (dbgInputShape logs the wrapper).
function normalizeToolArgsToSchema(name, input, schema) {
  try {
    if (!input || typeof input !== "object" || Array.isArray(input)) return input;
    const props = schema && schema.properties && typeof schema.properties === "object" ? schema.properties : {};
    let out = input;
    for (const k of Object.keys(input)) {
      const v = input[k];
      if (v == null || typeof v !== "object" || Array.isArray(v)) continue;
      let coerced = unwrapPrimitiveLikeObject(v); // (1) universal: a boxed primitive (any arg)
      if (coerced === undefined) {                // (2) schema-driven: a typed-primitive param wrapped as {text}/{value}/…
        const ps = props[k];
        const want = ps && typeof ps.type === "string" ? ps.type : null;
        if (want === "string" || want === "number" || want === "boolean") coerced = extractScalarFromWrapper(v, want, k);
      }
      if (coerced !== undefined) {
        if (out === input) out = { ...input };
        out[k] = coerced;
        dbg("normalizeToolArgs coerced wrapper -> scalar", "tool=" + name, "arg=" + k, "wrapper=" + safeJson(v).slice(0, 160));
      }
    }
    return out;
  } catch { return input; }
}

// CURSOR_COMPOSER_TOOL_ARG_CONTRACT (default ON; "0"/"false"/"off"/"no" to disable) gates the schema-derived
// argument contract appended to each tool's MCP description below. Disable only to shrink the tools/list payload;
// the per-tool EXTRAS (critical conflation fixes) still apply when disabled.
const TOOL_ARG_CONTRACT_ENABLED = !["0", "false", "off", "no"].includes(String(process.env.CURSOR_COMPOSER_TOOL_ARG_CONTRACT || "").toLowerCase());
// argContract reinforces a tool's exact arg KEYS/types, which only matters for the few tools composer CONFLATES
// (it borrows the Agent tool's object shape onto the workflow agent() function, invents Agent args, etc.). Every
// other tool (chrome-devtools, Read, Bash, Write, …) already conveys its shape via the schema, so the contract
// would just bloat the advertised description with no behavior gain — gate it to the handful that need it (~54 -> 3+).
const ARG_CONTRACT_TOOLS = new Set(["Workflow", "Agent"]);
function toolNeedsArgContract(name) {
  return ARG_CONTRACT_TOOLS.has(name) || (typeof name === "string" && name.startsWith("Task"));
}

// TOOL_USAGE_EXTRAS carries per-tool calling guidance the JSON schema alone CANNOT express — chiefly the calling
// convention of a function that lives INSIDE a string argument (Workflow.script's agent()), a recurring conflation
// source (composer borrows the `Agent` TOOL's object shape for the workflow `agent()` function -> "[object Object]"
// agents). Keyed by exact tool name; a tool not listed gets only the generic schema-derived contract.
const TOOL_USAGE_EXTRAS = {
  // Prescriptive against CC's ACTUAL workflow runtime (verified in the 2.1.158 binary): the script is AST-parsed —
  // `export const meta` (statement #1, a pure literal) is taken, then scriptBody = everything AFTER it is wrapped in
  // an async function with agent()/parallel()/pipeline()/phase() injected. Each rule below maps to a real failure
  // mode we've observed (export-in-body -> "Unexpected keyword export"; object-shape agent() -> "[object Object]";
  // bad agentType -> "agent type not found"; promises in parallel -> "expects an array of functions").
  Workflow:
    "DECOMPOSE BY REASONING ABOUT THIS TASK — derive the structure from the problem in front of you; do NOT copy a fixed recipe. Work it out in four moves:\n" +
    "  (1) SEAMS → THE PROBE PHASE, the HEART of the workflow (it holds the MOST agents). Name the INDEPENDENT pieces of THIS task that can run AT THE SAME TIME, then fan out ONE agent PER seam with `parallel([...])`. STRICT: the probe MUST have ≥ as many lanes as the seams you list AND ≥ any count the user named — '8-fold audit' / 'use 8 subagents' ⇒ EXACTLY 8 probe lanes. The wide probe IS the work; verify/synthesize are small add-ons AFTER it. If you can't name 4+ seams, cut finer (per-file / per-hypothesis / per-case). NEVER collapse the probe into one 'auditor'/'reviewer'/'skeptic' agent — the most common failure. Use SEAM PATTERNS (below) to find the split.\n" +
    "  SEAM PATTERNS — match your task to one, list the items, fan out ONE probe lane per item:\n" +
    "    • audit / review code → one lane per (file or module) × (lens: correctness, concurrency, security, error-handling), + 1–2 lanes attacking the core assumption  [e.g. 6 files × 4 lenses, or 8 invariants ⇒ 8 lanes]\n" +
    "    • hunt bugs across a repo → one lane per subsystem, OR per bug-class hypothesis (races, leaks, injection, off-by-one, auth-bypass)\n" +
    "    • build a feature / UI → one lane per component / screen / layer, then a final integration lane\n" +
    "    • design / architecture decision → one lane per COMPETING APPROACH, + one lane per stress-lens (scale, failure, security) attacking the front-runner\n" +
    "    • migrate / refactor → one lane per call-site or per file touched\n" +
    "    • research a question → one lane per sub-question or per source/doc\n" +
    "    • review a diff / PR → one lane per changed file × dimension (bugs, tests, perf)\n" +
    "    • fix failing tests → one lane per failing test\n" +
    "    • make a cleanup / refactor PLAN → one lane per category (dead code, duplication, complexity hotspots, config, test gaps)\n" +
    "  (2) PER-LANE CONTRACT + DEEP PROMPTS — each subagent is ISOLATED: it ONLY sees its `agent('…')` string, not your chat. Write `const CTX = 'TASK:… SCOPE:… GOAL:… METHOD:… OUTPUT: schema only BAR:…'` once, then a `function lanePrompt(unit) { return CTX + '\\nLANE: ' + unit + '\\n' + /* slice: paths/hypothesis, numbered steps, cite file:line, return schema */ }` and call `agent(lanePrompt(u), { schema, label })` in probe `parallel`. Probe prompts should be ~150–400+ chars; one-liners ('Audit auth') starve subagents. Every probe lane needs a `schema` (e.g. { findings: [{ where, issue, severity }] }). Verify/synth prompts can stay shorter — they receive structured JSON from prior phases.\n" +
    "  (3) VERIFY — a SEPARATE phase that runs AFTER the probe, ON the probe's FINDINGS. It is an ADD-ON that scrutinizes the probe's output; it does NOT replace or shrink the wide probe. Fan out one skeptic PER finding, schema'd ({ isReal, reasoning }), told to DEFAULT to refuted and confirm only with evidence; then `.filter(v => v.isReal)`. The skeptics are DOWNSTREAM and one-per-finding — do NOT turn the whole task into a single 'skeptic' or 'adversarial' agent; that discards the probe and is wrong.\n" +
    "  (4) SHAPE & THREAD — the result is: WIDE PROBE (one lane per seam / per the user's count) → VERIFY (one skeptic per finding) → synthesize (a lone agent reading the CONFIRMED set). Thread each phase's STRUCTURED output into the next. THESE ARE ALL WRONG: a workflow with ≤2 total agents; a probe phase with FEWER lanes than your seams or the user's number; collapsing the work into a single 'auditor'/'skeptic' agent; a bare verify+synthesize with no wide probe. Sketch the seam LIST first — the probe gets ONE lane per item on it; verify and synthesize are appended AFTER and never shrink it.\n" +
    "SELF-CHECK before emit: probe `parallel` lane count === UNITS.length === user N if given ('8-fold' ⇒ 8; if 1–2 lanes, you collapsed). `const CTX` is a full brief (not a stub); probe uses `lanePrompt(u)` or CTX+'\\nLANE:'+u with ~150+ chars per lane (not one-liners). Every probe has `schema`; separate verify phase after probe.\n" +
    "DELEGATE — when you launch this workflow to do work, the WORKFLOW'S AGENTS do it, not you. The tool returns IMMEDIATELY while they run in the BACKGROUND; that is NOT a cue to start editing or building yourself. Launch it, WAIT for it to finish, then synthesize its output. Doing the same work in the main agent in parallel wastes tokens and creates conflicting edits to the same files; if the user asked you to fan out, fanning out is the WHOLE job — do not also hand-apply the changes. Do NOT do the work yourself 'to be safe' or 'to make sure it completes end-to-end' — that exact reasoning IS the mistake; trust the workflow and WAIT for it.\n" +
    "WORKFLOW `script` RULES — the runtime PARSES your script; breaking any one fails the launch:\n" +
    "1. The FIRST statement MUST be `export const meta = { name, description, phases }`, a PURE LITERAL (no variables, function calls, spreads, or template strings inside meta; name + description are non-empty strings; phases is an array, e.g. [{ title: 'Scan' }]).\n" +
    "2. Use `export` EXACTLY ONCE — only for meta. Everything AFTER meta is the BODY; the runtime wraps it in an async function and runs it for you. So NEVER write `export` again (no `export function`, no `export default`, no exported helpers) and do NOT wrap the body in `async function` or an IIFE yourself — a second `export` or a self-wrap throws 'Unexpected keyword export' and the workflow never launches. Use `await` directly in the body.\n" +
    "3. `phase('title')` takes ONLY a string title and runs NO callback. NEVER `phase('name', () => {...})` or `phase('name', async () => {...})` — the callback is never called, so NO agents spawn (a 0-agent empty workflow). Call `phase('name')` on its own to label a section, then write the `agent()`/`parallel()` calls DIRECTLY in the body.\n" +
    "4. `agent()` is POSITIONAL: `agent(promptString, { label, schema, model, agentType })` — the prompt STRING is the FIRST arg. NEVER `agent({ description, prompt, subagent_type })` (that single-object shape is the `Agent` TOOL's, not this function's — it makes the agent's prompt `[object Object]`). If you set `agentType`, copy an EXACT registered agent name verbatim, case-sensitive (e.g. 'general-purpose', 'Explore', 'Plan'); 'explore' or 'generalPurpose' are rejected as 'agent type not found'.\n" +
    "5. `parallel()`/`pipeline()` take ONE array of THUNKS: `parallel([() => agent(...), () => agent(...)])`, or from a list `parallel(UNITS.map((u) => () => agent(...)))` — the INNER `() =>` is REQUIRED. NEVER bare `agent(...)` (a promise), NEVER `.map((u) => agent(...))` (returns promises — add the `() =>`), NEVER separate args (pass one array). On a `parallel`/`pipeline` error, FIX the thunks and RE-INVOKE Workflow — do not fall back to doing the task yourself.\n" +
    "6. No markdown fences. COPY THIS SHAPE EXACTLY (swap in YOUR derived seams + schema fields) — it does the four moves above: derives the SEAMS (UNITS), gives each lane a STRUCTURED-OUTPUT `schema`, adversarially VERIFIES every finding (default-refuted) and keeps only survivors, and threads the confirmed set into the synthesis. It also avoids every pitfall (one `export`; each `phase('x')` is a bare label; `parallel` gets `() =>` thunks; every `agent` is positional):\n" +
    "export const meta = { name: 'audit', description: 'Probe seams, adversarially verify, synthesize', phases: [{ title: 'probe' }, { title: 'verify' }, { title: 'synthesize' }] }\n" +
    "const CTX = 'TASK: audit THIS subsystem. SCOPE: <dirs in/out>. GOAL: bugs, races, security. METHOD: read, grep, targeted tests. OUTPUT: schema only. BAR: file:line + trigger + severity per finding.'\n" +
    "const UNITS = ['auth', 'session', 'routing', 'storage', 'streaming', 'usage']  // seams FIRST (8-fold ⇒ 8 items); one probe lane each\n" +
    "function lanePrompt(u) { return CTX + '\\nLANE: ' + u + '\\nSlice: only this seam. Steps: (1) map entrypoints (2) hunt races/leaks/auth (3) record findings file:line+trigger+severity. Return FIND schema; empty OK.' }\n" +
    "const FIND = { type: 'object', additionalProperties: false, required: ['findings'], properties: { findings: { type: 'array', items: { type: 'object', additionalProperties: false, required: ['where', 'issue', 'severity'], properties: { where: { type: 'string' }, issue: { type: 'string' }, severity: { type: 'string', enum: ['high', 'medium', 'low'] } } } } } }\n" +
    "const VERDICT = { type: 'object', additionalProperties: false, required: ['isReal', 'reasoning'], properties: { isReal: { type: 'boolean' }, reasoning: { type: 'string' } } }\n" +
    "const REFUTE = 'Try to REFUTE this finding. DEFAULT isReal=false; confirm only with concrete evidence: '\n" +
    "phase('probe')\n" +
    "const found = (await parallel(UNITS.map((u) => () => agent(lanePrompt(u), { label: 'probe:' + u, schema: FIND, agentType: 'general-purpose' })))).filter(Boolean).flatMap((r) => r.findings)\n" +
    "phase('verify')\n" +
    "const confirmed = (await parallel(found.map((f) => () => agent(REFUTE + JSON.stringify(f), { label: 'verify', schema: VERDICT, agentType: 'general-purpose' }).then((v) => ({ finding: f, ok: v && v.isReal }))))).filter((x) => x && x.ok).map((x) => x.finding)\n" +
    "phase('synthesize')\n" +
    "return await agent(CTX + '\\nFINAL: Rank these CONFIRMED findings into one report: ' + JSON.stringify(confirmed), { label: 'synth', agentType: 'general-purpose' })",
  Agent:
    "IMPORTANT — `Agent` (capitalized, THIS tool) and the workflow `agent()` function are DIFFERENT. `Agent` is invoked as a tool call with the object arguments above; `subagent_type` must be an EXACT registered agent name copied verbatim, case-sensitive (e.g. 'general-purpose', 'Explore', 'Plan') — NEVER 'explore' or 'generalPurpose'. The lowercase `agent()` is a SEPARATE positional function (`agent(promptString, {opts})`, e.g. inside a Workflow `script`); never give it this tool's object shape. DELEGATE: when you spawn an `Agent` to DO work, let IT do that work — do NOT also make the same edits or run the same commands yourself in the main agent while it runs; wait for its result and build on it.",
  Bash:
    "BACKGROUND-TASK AWARENESS — a Bash command running in the BACKGROUND (you backgrounded it, or the user pressed ctrl-b) is STILL RUNNING; its result has not arrived yet. Do NOT launch the same command again while it is in flight — especially long ones like builds, servers, or test suites: a second or third concurrent build corrupts shared build artifacts and wastes the machine. Wait for it to finish, or read its existing output handle to check progress; only re-run a command once it has actually exited.",
};

// augmentWorkflowResultOnFailure (failure-feedback, "A"): when CC's Workflow tool returns a SYNTAX error or a
// 0-agent empty run, append a targeted fix — the specific reason + the full prescriptive script contract — to the
// result composer reads, so its NEXT move corrects the actual mistake (far higher-signal than the static tool
// description it has been ignoring). Fail-safe: only a RECOGNIZED failure is augmented; a real workflow result
// (agents ran) passes through untouched, and any error returns the content unchanged.
// BACKGROUND_LAUNCH_RE matches the "I started it; it is running in the background" notice a tool returns when its
// work was BACKGROUNDED — the Workflow tool (which always runs async) and a backgrounded Bash command. composer
// reads such a result and, instead of waiting, either relaunches the command (the duplicate-concurrent-builds bug)
// or hand-does the work itself in the main agent "to be safe" (the fan-out-then-also-do-it-yourself anti-pattern).
const BACKGROUND_LAUNCH_RE = /running in (the )?background|\/workflows to monitor|running in background with id|started in the background/i;

// augmentBackgroundLaunchResult appends a LIVE, model-visible interrupt to a "running in the background" tool
// result so composer WAITS for it instead of relaunching it or redoing its work. This rides the tool RESULT (not
// the cached tool description), so it reaches the model the very turn it is deciding what to do next — far stronger
// than a description nudge composer rationalizes away. It names the TOOL and the extracted task/run id, so when
// several things are running concurrently it is unambiguous WHICH one this is about. Fail-safe: only a STRING
// result that matches the pattern and is not already augmented is touched; everything else passes through. Idempotent.
function augmentBackgroundLaunchResult(content, toolName) {
  try {
    if (typeof content !== "string" || !content) return content;
    if (content.includes("[BRIDGE] STILL RUNNING")) return content; // never double-append
    if (!BACKGROUND_LAUNCH_RE.test(content)) return content;
    const idm =
      content.match(/\bwf_[a-z0-9-]{4,}\b/i) ||                                  // workflow run id
      content.match(/\b(?:bash|task|run|proc|shell)_[a-z0-9-]+\b/i) ||           // bash_1 / task_x handle
      content.match(/\b(?:id[:=]|with id\s+)\s*["']?([a-z0-9][a-z0-9_-]{2,})/i); // "id: xyz" / "with id xyz"
    const id = idm ? (idm[1] !== undefined ? idm[1] : idm[0]).trim() : "";
    const who = (toolName ? "the `" + toolName + "` you launched" : "this") + (id ? " (id: " + id + ")" : "");
    return content + "\n\n[BRIDGE] STILL RUNNING IN THE BACKGROUND — " + who + " is NOT finished. Do NOT launch it again, and do NOT redo its work yourself in the meantime (no parallel edits, builds, or commands for what it is handling). WAIT for it to complete, then use its result. Re-running it or doing the work yourself duplicates effort and causes conflicts.";
  } catch { return content; }
}

function augmentWorkflowResultOnFailure(content, isError) {
  try {
    const text = typeof content === "string" ? content : safeJson(content);
    const t = (text || "").toLowerCase();
    let reason;
    if (t.includes("is not a function") || t.includes("not iterable") || ((t.includes("parallel") || t.includes("pipeline")) && (t.includes("error") || t.includes("thunk") || t.includes("function")))) {
      reason = "`parallel()`/`pipeline()` got the WRONG argument — each takes ONE ARRAY of THUNKS (zero-arg functions): `parallel([() => agent('a',{…}), () => agent('b',{…})])`, and from a list `parallel(UNITS.map((u) => () => agent(…)))` — the INNER `() =>` is REQUIRED. NEVER pass bare `agent(...)` (those are promises, not functions), NEVER spread them as separate args (`parallel(()=>…, ()=>…)`), and NEVER `.map((u) => agent(…))` (that returns promises — you MUST add the `() =>`: `.map((u) => () => agent(…))`)";
    } else if (isError || t.includes("unexpected keyword") || t.includes("syntax error") || t.includes("not launched")) {
      reason = "the script had a SYNTAX error — usually `export` used a SECOND time, or the body wrapped in a function: the runtime already wraps the body in an async function, so use `export` ONLY for `meta` and `await` directly";
    } else if (t.includes("0 agent") || t.replace(/\s/g, "").includes('agentcount":0') || /completed in 0s\b/.test(t)) {
      reason = "it spawned 0 AGENTS — almost always `phase('x', () => {...})`: `phase()` takes ONLY a title and runs NO callback, so the agent()/parallel() calls must be DIRECTLY in the body";
    } else {
      return content; // not a recognized failure -> leave the real result untouched
    }
    return text + "\n\n[BRIDGE] Your Workflow call FAILED — " + reason + ".\nYou MUST FIX the `script` and RE-INVOKE the `Workflow` tool NOW. Do NOT abandon the workflow to do the task yourself / inline / with the Task tool — this is a SMALL mechanical script correction, not a reason to give up on the workflow. Keep your phases, lanes, and schema; just apply the one fix and re-run `Workflow` with the corrected `script`, following these rules EXACTLY:\n\n" + TOOL_USAGE_EXTRAS.Workflow;
  } catch {
    return content;
  }
}

// WORKFLOW_AGENT_TYPES: the standard CC registered agent names. snapWorkflowAgentTypes rewrites a known-WRONG
// agentType/subagent_type VALUE in a Workflow script to its exact registered name (e.g. 'generalPurpose' ->
// 'general-purpose', 'explore' -> 'Explore') so a workflow does not fail with "agent type '...' not found".
// Conservative: only an UNAMBIGUOUS case/punctuation variant of a known name is snapped; a custom/unknown agent
// name is left untouched. The model is inconsistent about this value run-to-run, and it lives INSIDE the script
// string (so the bridge can't schema-snap it like a top-level tool arg — a targeted value rewrite is the lever).
const WORKFLOW_AGENT_TYPES = ["claude", "claude-code-guide", "codex:codex-rescue", "Explore", "general-purpose", "Plan", "statusline-setup"];
function snapAgentTypeValue(v) {
  if (typeof v !== "string" || WORKFLOW_AGENT_TYPES.includes(v)) return null; // not a string, or already exact
  const norm = (s) => s.toLowerCase().replace(/[^a-z0-9]/g, "");
  const nv = norm(v);
  const m = WORKFLOW_AGENT_TYPES.filter((c) => norm(c) === nv);
  return m.length === 1 ? m[0] : null; // unambiguous case/punctuation variant -> the exact registered name
}
function snapWorkflowAgentTypes(script) {
  try {
    if (typeof script !== "string") return script;
    return script.replace(/\b(agentType|subagent_type)(\s*:\s*)(['"`])([A-Za-z0-9_:-]+)\3/g, (full, key, sep, q, val) => {
      const snapped = snapAgentTypeValue(val);
      return snapped ? key + sep + q + snapped + q : full;
    });
  } catch { return script; }
}

// CURSOR_COMPOSER_USER_MSG_REMINDER (default empty = off): when set, its text is appended to the END of every
// NON-EMPTY user message sent to the model (driveUserSend) — e.g. a standing "re-read the rules / tool contract
// this turn" nudge that rides every turn live, instead of only the tool descriptions (which cache at session start).
const USER_MSG_REMINDER = (process.env.CURSOR_COMPOSER_USER_MSG_REMINDER || "").trim();
function appendRulesReminder(userText, reminder = USER_MSG_REMINDER) {
  if (!reminder || typeof userText !== "string" || userText.length === 0) return userText;
  return userText + "\n\n" + reminder;
}

// argContractFor builds one concise, unambiguous "how to call this tool" sentence FROM the tool's own inputSchema,
// so composer-2.5 uses the EXACT argument keys/types this tool declares and never borrows another tool's shape
// (the root of the agent({...}) conflation, and a nudge against scalars arriving object-wrapped). Schema-driven, so
// it applies to ANY tool with zero per-tool authoring. Returns "" when the schema declares no properties.
function argContractFor(name, schema) {
  try {
    const props = schema && schema.properties && typeof schema.properties === "object" ? schema.properties : null;
    if (!props) return "";
    const keys = Object.keys(props);
    if (!keys.length) return "";
    const req = new Set(Array.isArray(schema.required) ? schema.required : []);
    const list = keys.map((k) => {
      const ty = props[k] && typeof props[k].type === "string" ? props[k].type : "any";
      return "`" + k + "` (" + ty + (req.has(k) ? ", required" : "") + ")";
    });
    return "Call `" + name + "` with exactly these argument keys, each value as its declared JSON type — a scalar as a plain scalar, never wrapped in an object: " + list.join(", ") + ".";
  } catch { return ""; }
}

// augmentToolDescription appends the schema-derived argument contract (+ any per-tool extra) to a tool's MCP
// description. Injected ONLY into what composer reads (mcpToolsForServer -> tools/list + mcpState); the internal
// session.advertise description stays untouched. Fail-safe: returns the base description on any problem.
// TOOL_USAGE_PROMINENT is PREPENDED to a tool's description (the FIRST thing the model reads) for the few tools
// where one mistake breaks everything. For Workflow it leads, unmissably, with the two CONFIRMED failure modes —
// object-shape agent() and bare-call parallel — as explicit ✅RIGHT / ❌WRONG contrasts so there is zero ambiguity.
const TOOL_USAGE_PROMINENT = {
  Workflow:
    "━━━━━━━━━━ WORKFLOW SCRIPT — READ THIS FIRST ━━━━━━━━━━\n" +
    "MENTAL MODEL (the root mistake): the script is a PLAIN imperative async body — NOT a declarative/phased framework. Pass data between steps with NORMAL `const` variables + top-level `await`. `phase('title')` and `meta.phases` are PROGRESS LABELS ONLY — they run NO callback, inject NO data (`{ prev }` is NEVER passed to a 'next phase'), and define NO execution graph. Write `const found = await parallel([...]); const out = await agent('Synthesize: ' + JSON.stringify(found))` — NEVER `phase('x', () => …)` (the callback never runs), NEVER `phase('y', ({ found }) => …)` (no injection), NEVER infer an execution order from `meta.phases` (it's UI metadata).\n" +
    "PROBE WIDE — list seams in UNITS[], then `parallel` one agent PER item (probe holds MOST agents; user '8-fold' ⇒ 8 lanes). NEVER one auditor/skeptic replacing the probe. DEEP PROMPTS — subagents ONLY see `agent('…')`; write `const CTX` + `function lanePrompt(u){ return CTX+'\\nLANE:'+u+'\\n…' }` (~150+ chars per lane); one-liners starve them. RIGOR — `schema` on every probe; separate verify (one skeptic per finding, default-refuted, `.filter`). 3+ phases typical. Details below.\n" +
    "DELEGATE, DON'T DOUBLE — the Workflow tool RETURNS IMMEDIATELY and runs in the BACKGROUND. That does NOT mean it finished, and it does NOT mean you should do the work yourself. Its agents ARE doing the work; WAIT for the completion notification, then use their results. NEVER make the same edits or run the same commands in the main agent while a workflow (or a subagent you spawned) is still running — it duplicates the work and produces conflicting changes. If the user asked you to fan out, fanning out IS the job: do not also hand-apply the changes. Do NOT do it yourself 'to be safe' or 'to make sure it completes' — that exact reasoning is the mistake.\n" +
    "Then two mistakes that BREAK every workflow:\n" +
    "(1) `agent()` is a POSITIONAL function — the prompt STRING is the FIRST argument:\n" +
    "      ✅ RIGHT:  agent('Audit the auth code for bugs', { agentType: 'general-purpose' })\n" +
    "      ❌ WRONG:  agent({ description: '…', prompt: '…', subagent_type: '…' })\n" +
    "                 An OBJECT first arg makes the prompt literally '[object Object]' and the agent does nothing.\n" +
    "(2) `parallel()` / `pipeline()` take ONE ARRAY of THUNKS — wrap each call as `() => agent(...)`:\n" +
    "      ✅ RIGHT:  await parallel([ () => agent('a', { agentType: 'general-purpose' }), () => agent('b', { agentType: 'general-purpose' }) ])\n" +
    "      ✅ RIGHT:  await parallel(UNITS.map((u) => () => agent(lanePrompt(u), { schema: FIND })))   // from a list: note the INNER () =>\n" +
    "      ❌ WRONG:  await parallel([ agent('a', {…}), agent('b', {…}) ])                            // promises, not functions\n" +
    "      ❌ WRONG:  await parallel(UNITS.map((u) => agent(lanePrompt(u), {…})))                     // .map returns promises — add the () =>\n" +
    "      ❌ WRONG:  await parallel(() => agent('a'), () => agent('b'))                              // pass ONE array, not separate args\n" +
    "                 A thunk is a ZERO-ARG function `() => agent(...)`; bare `agent(...)` is a promise and errors immediately.\n" +
    "(3) Subagents do NOT see your chat — put the brief IN the script:\n" +
    "      ✅ RIGHT:  const CTX='TASK:…'; function lanePrompt(u){ return CTX+'\\nLANE:'+u+'\\nSteps:…' }; agent(lanePrompt('auth'), { schema:FIND })\n" +
    "      ❌ WRONG:  agent('Audit auth')   // blind one-liner — subagent has no context\n" +
    "(4) Each STEP needs its OWN `phase('title')` placed BEFORE that step's agents — every agent attaches to the MOST RECENT `phase()`:\n" +
    "      ✅ RIGHT:  phase('probe'); await parallel([...]); phase('synthesize'); await agent('Synthesize the findings…', { agentType: 'general-purpose' })\n" +
    "      ❌ WRONG:  phase('probe'); await parallel([...]); await agent('Synthesize…')   // the synth agent lands inside 'probe' — call phase('synthesize') FIRST\n" +
    "                 So a 2nd/3rd step's agents do NOT pile into the 1st step. List EVERY step in meta.phases:[{title:'probe'},{title:'synthesize'}] with titles matching your phase() calls.\n" +
    "IF A WORKFLOW CALL ERRORS: it is a SMALL mechanical fix (one of the above) — CORRECT the `script` and RE-INVOKE `Workflow`. NEVER abandon the workflow to do the task inline / yourself / with the Task tool just because the script errored.\n" +
    "WHILE A WORKFLOW RUNS: do NOT busy-poll it with `sleep`/`stat`/`tail` loops — you are AUTO-NOTIFIED the moment it completes (use `/workflows` for live progress). Burning turns on poll loops makes you UNRESPONSIVE: a new user message must be answered RIGHT AWAY, not after the loop. If the user asks something while a workflow runs, STOP polling and reply.\n" +
    "SKELETON — the WHOLE shape, copy and adapt (every line matters; this is the imperative model above):\n" +
    "  export const meta = { name: 'task', description: 'what it does', phases: [{ title: 'probe' }, { title: 'synthesize' }] }\n" +
    "  const CTX = 'TASK: <the goal>.  SCOPE: <in/out>.  OUTPUT: <what each lane returns>.'\n" +
    "  const UNITS = ['partA', 'partB', 'partC']            // the INDEPENDENT pieces to fan out — one lane each (>= any N the user named)\n" +
    "  phase('probe')\n" +
    "  const results = await parallel(UNITS.map((u) => () => agent(CTX + '\\nLANE: ' + u + '\\nDo the work for THIS piece and report.', { agentType: 'general-purpose' })))\n" +
    "  phase('synthesize')\n" +
    "  return await agent(CTX + '\\nMerge these lane results into the final answer: ' + JSON.stringify(results), { agentType: 'general-purpose' })\n" +
    "(Add a `schema` per lane + a verify phase for audits — see the full example below.)\n" +
    "Copy the ✅ RIGHT forms exactly. Full rules and a complete runnable example follow below.\n" +
    "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━",
};

function augmentToolDescription(name, description, schema) {
  try {
    const base = typeof description === "string" ? description : "";
    const prominent = (TOOL_USAGE_PROMINENT && TOOL_USAGE_PROMINENT[name]) || ""; // PREPENDED: read first, before CC's own docs
    const extra = (TOOL_USAGE_EXTRAS && TOOL_USAGE_EXTRAS[name]) || "";
    const contract = (TOOL_ARG_CONTRACT_ENABLED && toolNeedsArgContract(name)) ? argContractFor(name, schema) : "";
    const parts = [prominent, base, contract, extra].filter(Boolean);
    return parts.join("\n\n");
  } catch { return typeof description === "string" ? description : ""; }
}

// augmentUnderspecifiedToolSchema constrains a known Claude Code tool whose advertised schema under-specifies
// its REQUIRED inputs, so composer-2.5 constructs a valid call. The Workflow tool ships with NO required field
// (verified against the CC 2.1.158 binary: its args are script/scriptPath/name/args/resumeFromRunId/description/
// title, and the workflow SOURCES script/scriptPath/name are ALTERNATIVES so none is individually required) —
// composer reads that as "a title-only call is valid", omits the script, and the workflow runs nothing. We add
// an anyOf requiring AT LEAST ONE of `script` (inline) OR `scriptPath` (written to disk first) — the two ways to
// PROVIDE a workflow — without forcing either, and WITHOUT dropping/altering any of the 7 verbatim args (the
// schema is spread through unchanged; we only add the anyOf constraint). Defensive: only Workflow, only when a
// script/scriptPath property exists, only when nothing is already required and no combinator is present; every
// other tool/schema passes through byte-for-byte (fail-safe — a throw or unexpected shape returns the input).
function augmentUnderspecifiedToolSchema(name, schema) {
  try {
    if ((name || "") !== "Workflow") return schema;
    if (!schema || typeof schema !== "object" || !schema.properties || typeof schema.properties !== "object") return schema;
    const hasScript = !!schema.properties.script;
    const hasScriptPath = !!schema.properties.scriptPath;
    if (!hasScript && !hasScriptPath) return schema; // unexpected shape -> leave untouched
    if (Array.isArray(schema.required) && schema.required.length) return schema; // already constrained
    if (schema.anyOf || schema.oneOf || schema.allOf) return schema; // already has a combinator -> never clobber
    const branches = [];
    if (hasScript) branches.push({ required: ["script"] });
    if (hasScriptPath) branches.push({ required: ["scriptPath"] });
    // Bind the highest-signal script-shape rules to the `script` ARG ITSELF — composer reads a property's
    // description at the moment it constructs that value, so the rules land right where the mistakes happen
    // (a concise mirror of the full guidance in the tool description). Prepended to CC's own script description.
    if (hasScript && schema.properties.script && typeof schema.properties.script === "object") {
      const RULES =
        "INLINE JS WORKFLOW — a PLAIN imperative async body, NOT a declarative/phased framework. " +
        "`export const meta = { name, description, phases: [{ title }] }` FIRST (the ONLY `export`), then top-level `await`; pass data between steps with normal `const` variables. " +
        "`phase('title')` and `meta.phases` are PROGRESS LABELS ONLY — no callback, no `{prev}` injection, no execution graph. " +
        "`agent('prompt string', { agentType })` is POSITIONAL (the string is the FIRST arg, never an object). " +
        "`parallel()`/`pipeline()` take ONE ARRAY of THUNKS: `parallel(UNITS.map((u) => () => agent(...)))` — the INNER `() =>` is REQUIRED, never bare `agent(...)`. " +
        "On any error, FIX the script and RE-INVOKE Workflow — do not abandon it to do the task inline. " +
        "EXAMPLE: `export const meta={name:'t',description:'d',phases:[{title:'probe'},{title:'synthesize'}]}; const r=await parallel(UNITS.map((u)=>()=>agent('do '+u,{agentType:'general-purpose'}))); return await agent('merge: '+JSON.stringify(r),{agentType:'general-purpose'})`";
      const ccDesc = typeof schema.properties.script.description === "string" && schema.properties.script.description ? "\n\n" + schema.properties.script.description : "";
      const script = { ...schema.properties.script, description: RULES + ccDesc };
      return { ...schema, anyOf: branches, properties: { ...schema.properties, script } };
    }
    return { ...schema, anyOf: branches };
  } catch { return schema; }
}

// ADD-58: classify whether the HTTP peer is loopback. The server binds 127.0.0.1, but a reverse proxy can
// forward remote traffic to it; a forwarded request also carries X-Forwarded-For, which a same-host loopback
// curl never does. So "loopback" requires BOTH a loopback socket address AND no X-Forwarded-For header.
function isLoopbackRemote(req) {
  if (req && req.headers && req.headers["x-forwarded-for"]) return false;
  const a = (req && req.socket && req.socket.remoteAddress) || "";
  return a === "127.0.0.1" || a === "::1" || a === "::ffff:127.0.0.1" || a === "" /* unix socket / test */;
}
// ADD-105: classify a BIND-HOST string (the interface server.listen binds), mirroring the loopback addresses
// isLoopbackRemote treats as local. "localhost" resolves to loopback; an empty/absent host binds all
// interfaces (treated as NON-loopback, the conservative choice). Used by validateBindHost.
function bindHostIsLoopback(host) {
  const h = String(host || "").trim().toLowerCase();
  if (h === "") return false; // empty host = bind all interfaces (NOT loopback)
  return h === "127.0.0.1" || h === "::1" || h === "::ffff:127.0.0.1" || h === "localhost";
}
// resolveBridgeHost returns the configured bind host (defaulting to 127.0.0.1). Pure (testable).
function resolveBridgeHost(host = BRIDGE_HOST) { return String(host || "").trim() || "127.0.0.1"; }
// validateBindHost (ADD-105) enforces the secure-by-default bind policy. A loopback bind is always allowed. A
// NON-loopback bind exposes /agent/turn (which carries the per-user Cursor bearer) on the network, so it
// REQUIRES multi-tenant auth (a BRIDGE_TOKEN) — otherwise it refuses to start (fail-closed) UNLESS the operator
// sets the explicit insecure opt-in. Returns { ok, warn?, error? }: `error` (when present) means refuse to
// start; `warn` is an always-surfaced plaintext-exposure caution on any non-loopback bind. Pure (testable).
function validateBindHost(host, hasToken, allowInsecure = false) {
  if (bindHostIsLoopback(host)) return { ok: true };
  if (!hasToken && !allowInsecure) {
    return { ok: false, error: `CURSOR_AGENT_BRIDGE_HOST=${host} binds a non-loopback interface but no CURSOR_AGENT_BRIDGE_TOKEN (multi-tenant auth) is set — refusing to start (the bearer would be exposed in plaintext). Set CURSOR_AGENT_BRIDGE_TOKEN, terminate TLS in front, or set CURSOR_AGENT_ALLOW_INSECURE_BIND=1 to override.` };
  }
  return { ok: true, warn: `CURSOR_AGENT_BRIDGE_HOST=${host} binds a non-loopback interface — /agent/turn is reachable over the network. Ensure TLS is terminated in front; the per-user Cursor bearer is sent in plaintext otherwise.` };
}
// healthBody returns the FULL diagnostic health payload (patched flag + live session count) to a loopback or
// bridge-token-authenticated caller, and a bare liveness {ok:true} to any remote/forwarded caller (ADD-58).
function healthBody(req) {
  const h = (req && req.headers) || {};
  const authed = BRIDGE_TOKEN && constEq(h["x-bridge-auth"], BRIDGE_TOKEN);
  if (isLoopbackRemote(req) || authed) return { ok: true, patched: true, sessions: sessions.size };
  return { ok: true };
}

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

  // Upstream rate-limit circuit breaker: while OPEN for this key, fast-fail NEW runs with a clear 429 so client
  // retries back off instead of re-poisoning the freshly-recycled HTTP/2 connection. tool_results continuations
  // are NOT gated — they complete a paused run, and blocking one would strand it until the abandonment watchdog.
  // HALF-OPEN after the window: the next new-user turn probes and closeBreaker (onRunComplete) clears it on success.
  if (input.type !== "tool_results") {
    const kh = keyHash(cursorKey);
    if (breakerOpen(kh)) {
      const waitS = Math.ceil(breakerRetryAfterMs(kh) / 1000);
      fail(429, `upstream is rate-limiting this account (Cursor HTTP/2 ENHANCE_YOUR_CALM); the proxy recycled the connection and is backing off — retry in ~${waitS}s and avoid rapid retries (they re-trip the limit)`);
      return;
    }
  }

  // Enforced response constraints + tool_choice carried from the Go executor (Comment 3). Applied as
  // model instructions and tool-advertisement gating on the user turn. unsupportedHardGuarantees (H20/H21/
  // ADD-72/ADD-84) is the executor's advisory list of constraints the composer path cannot hard-enforce; it is
  // threaded through here so BOTH the send path (driveUserSend) AND the ADD-77/ADD-83 resume-injection surface
  // it to the model identically (never a claim of hard enforcement).
  const constraints = { toolChoice: body.toolChoice || "", responseFormat: body.responseFormat, stop: body.stop, maxTokens: body.maxTokens, unsupportedHardGuarantees: body.unsupportedHardGuarantees };

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
    refreshSessionFromBody(session, body);
    // ORPHAN GUARD (P0 — lost-owner reseed): the session exists but issued NONE of the ids in this batch, and
    // there is no trailing user payload to drive a fresh send. This is the mis-route the bug produced — the run
    // that owned these tool calls died, the Go executor cleared its ownership (forgetSession on the error stop),
    // and the result was routed by lineage to a session that cannot own it. Forwarding it would emit an
    // "unknown tool_call_id" error over HTTP 200 (headers already written at the activeRes/runTurn writeHead),
    // which the client retries forever. Headers are NOT written yet here, so return 410 — the same
    // lost-continuation signal as the `!session` branch above — to drive the executor's reseed-on-410, which
    // replays the conversation as a fresh user turn (the orphaned result is already visible in that replayed
    // history, so no context is lost). A continuation WITH user payload is left alone: runTurn's C1 path
    // fresh-sends it and recovers on its own.
    // Scope: ANY non-streaming session (activeRes unset) — idle, OR a paused run with its own outstanding
    // pending, OR a session whose owning run just died and whose ownership the Go executor already forgot. For a
    // wholly-foreign no-user-payload batch ONLY, that batch can never be resolved here (allToolResultsForeign is
    // non-mutating and returns false the moment ANY id is pending/delivered/ever-emitted), so the sole useful
    // action is reseed-recovery (410); the foreign id is never resolved into a pending. An ACTIVELY-STREAMING
    // session (activeRes set) still surfaces a foreign id as an "unknown tool_call_id" error (C04: never reseed
    // mid-stream, never a silent misroute or false success). The old full-idle gate (pending.size===0) excluded
    // the paused / own-pending variant this bug produces under dynamic-workflow fan-out; (not activeRes) includes
    // it while still excluding the live stream. Safe because routing is CONTENT-derived (deriveComposerSessionID
    // over the continuation's own head/opener), so the orphan re-derives to its OWN just-died session id (lease
    // already released + ownership forgotten at the error stop, run ended/paused, activeRes null) or a fresh
    // reseed fork — never a sibling subagent's live stream.
    if (!session.activeRes) {
      const hasUserText = typeof input.userText === "string" && input.userText.length > 0;
      const hasUserImages = (Array.isArray(input.images) && input.images.length > 0) || collectToolResultImages(input).length > 0;
      if (!hasUserText && !hasUserImages && session.allToolResultsForeign(input.results)) {
        dbg("tool_results ALL-FOREIGN on non-streaming session + no user payload -> 410 reseed (orphaned: owning run died, ownership lost)",
          "session=" + sessionId, "ids=" + safeJson((input.results || []).map((r) => r && r.toolCallId)));
        fail(410, "orphaned tool_call_id: none of these results were issued by this session (the owning run has ended); the continuation must be re-seeded");
        return;
      }
    }
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
      const hasUserText = typeof input.userText === "string" && input.userText.length > 0;
      // The continuation may also carry NEW user images (top-level input.images and/or images folded into tool
      // results); like userText, these are user payload that must reach the model, not be dropped.
      const hasUserImages = (Array.isArray(input.images) && input.images.length > 0) || collectToolResultImages(input).length > 0;
      dbg("handleTurn tool_results CONCURRENT ack", sessionId, "matched=" + matched, "of=" + batchLen, "unknown=" + safeJson(unknown), "userText=" + hasUserText, "userImages=" + hasUserImages, "pendingAfter=" + session.pending.size);
      // ADD-36: a PARTIAL-PARALLEL mixed turn on the concurrent path — the client answered SOME tools
      // (matched>0), included new user text (hasUserText), but OTHER pendings remain unanswered
      // (pending.size>0, e.g. the client cancelled/backgrounded those tools). Without handling, the live run
      // stays blocked on the unresolved pendings until the abandonment watchdog, and the user's trailing
      // instruction (folded into a matched tool result by the executor) is stranded — a benign clean end_turn
      // would tell the client to stop while no answer is coming. Treat it as an interruption/redirect:
      // synthesize a MODEL-VISIBLE cancellation result for each unresolved pending (isError) so the live run
      // unblocks and resumes, consuming the folded user text. Never a clean end_turn that strands the work.
      if (hasUserText && matched > 0 && session.pending.size > 0) {
        const cancelIds = [...session.pending.keys()];
        dbg("handleTurn CONCURRENT partial-parallel + userText -> synthesize cancellations so the run resumes (ADD-36)", sessionId, "cancelling=" + safeJson(cancelIds));
        for (const pid of cancelIds) {
          session.resolvePending(pid, "tool call superseded by a new user instruction; it was not completed", true);
        }
      }
      // NOTE on input.userText (C1): on the concurrent path the live run already owns activeRes and is
      // streaming the model's answer; the executor folds any trailing user text into the last tool result
      // (C1 belt-and-suspenders), so it reaches the live run WHEN that result actually RESOLVES a pending
      // (matched>0). Driving a separate fresh send HERE would spawn a colliding/orphaned run, so the concurrent
      // path only acks. But when NOTHING matched (matched===0: every id was a benign re-ack / reaped / already-
      // resolved), the fold reached no pending and the in-flight stream began BEFORE this payload arrived — so a
      // trailing userText/images is NOT in the run anywhere. runTurn's twin handles this via the C1 redirect
      // (driveUserSend at the `hasUserPayload && noneToResume` branch); the concurrent path cannot drive a fresh
      // send without colliding with the live stream, so we must NOT clean-ack (which tells the client to stop
      // retrying while its instruction was silently lost). Surface a typed error so the client re-sends it.
      const userPayloadLost = (hasUserText || hasUserImages) && matched === 0 && unknown.length === 0;
      try {
        if (unknown.length > 0) {
          // Foreign/never-issued id: a genuine desync. Surface it (do NOT fake success) so the client sees it.
          res.write(formatSseData({ type: "turn_end", stop_reason: "error", error: `unknown tool_call_id ${unknown[0]}: not issued by this session` }));
        } else if (userPayloadLost) {
          // matched===0 + new user payload: the continuation's userText/images could not be delivered to the
          // in-flight run (it predates this payload, and the concurrent path cannot fresh-send). Error so the
          // client retries the instruction on a fresh turn — never a clean end_turn that drops it (finding #6).
          dbg("handleTurn CONCURRENT user payload could not reach the live run -> error ack so the client retries (finding#6)", sessionId, "userText=" + hasUserText, "userImages=" + hasUserImages);
          res.write(formatSseData({ type: "turn_end", stop_reason: "error", error: "a new user instruction or image arrived while a run was streaming and could not be delivered to it; resend it as a new turn" }));
        } else {
          // matched>0 -> resolved into the live run; matched===0 with no user payload -> all ids were benign
          // re-acks (already-resolved/duplicate). Either way a clean ack is correct (the run is live).
          res.write(formatSseData({ type: "turn_end", stop_reason: "end_turn" }));
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
  if (!session) {
    // ADD-63 (LOAD-SHED): reject a NEW session when at cap and nothing idle can be shed. Never evict a live or
    // paused session to admit this one. ADD-75: likewise reject a NEW tenant (new platform) at the platform cap.
    if (!sessionCapHasRoomForNew()) { fail(429, `session capacity reached (${MAX_SESSIONS}); all sessions are active or paused — retry later`); return; }
    if (MULTI_TENANT && !platformCapHasRoomForNew(cursorKey)) { fail(429, `platform capacity reached (${MAX_PLATFORMS}); all tenant platforms are in use — retry later`); return; }
    session = new Session(sessionId, cursorKey); sessions.set(sessionId, session); dbg("handleTurn NEW session", sessionId);
  }
  else {
    dbg("handleTurn REUSE session", sessionId, "runActive=" + !!session.run, "activeRes=" + !!session.activeRes, "waiters=" + session.waiters);
    // ADD-79 (bridge half): the upstream Cursor key may have changed for the SAME external session (tenant key
    // rotation, admin rebind, or multi-tenant forwarding a different per-user key under the same conversation
    // id). Compare a NON-REVERSIBLE fingerprint of the request key to the session's stored key fingerprint; if
    // they differ, rotate the durable agent onto the NEW key (tombstone the old-key agent, fresh agentId under a
    // new keyEpoch, re-seed) — NEVER keep answering on the old (possibly revoked / wrong-billed) account.
    // rotateForKeyChange sets session.cursorKey + rotates the durable agent atomically (never a live mutation of
    // the key without rotation). Defense-in-depth: the executor half folds the key fingerprint into the sess_id
    // so a key change normally arrives as a NEW session id; this catches the same-id case (either half is safe).
    if (keyFingerprint(cursorKey) !== keyFingerprint(session.cursorKey)) {
      dbg("handleTurn REUSE with CHANGED cursor key -> rotateForKeyChange (ADD-79)", sessionId, "oldKeyHash=" + keyHash(session.cursorKey), "newKeyHash=" + keyHash(cursorKey));
      await session.rotateForKeyChange(cursorKey);
    }
  }
  refreshSessionFromBody(session, body);
  // ADD-37 (extended by ADD-106): a plain NEW-USER turn that arrives while a run is still LIVE on this session
  // is an INTERRUPTION, not a concurrent generation — the user is steering, so the old run must yield to the
  // new instruction. Two sub-cases, both now interrupted:
  //   (a) the run is PAUSED awaiting client tool results (activeRes null, pendings outstanding). Without the
  //       interrupt the new turn would queue behind whenLogicalDone() and hang for PENDING_TIMEOUT_MS (minutes)
  //       until the abandonment watchdog reaps the missing results — the interrupt would appear to do nothing.
  //   (b) the run is ACTIVELY STREAMING (activeRes set). ADD-106 (Comment 2): previously this case fell through
  //       to enqueueTurn, which waits on session.tail + whenLogicalDone() with NO wall-clock timer — so if the
  //       prior run NEVER terminates (a wedged/never-ending upstream stream), the new user turn stays queued
  //       FOREVER. A genuine new-user turn must be able to interrupt a live stream too, so we cancel it first.
  // cancel() tears down activeRes + the live run, rejects every pending (so an awaiting tool promise fails and
  // the run terminates), fires notifyLogicalDone (advancing the FIFO), and bumps runEpoch — that epoch gate is
  // exactly what makes cancel-then-enqueue safe: the superseded run's late wait()/stream callbacks can no
  // longer leak into the successor turn. THEN enqueue the new turn so it runs immediately.
  //
  // GUARD RAILS: this only fires for a genuine NEW-USER turn — a tool_results CONTINUATION returned early above
  // (it COMPLETES the live run rather than interrupting it), and a utility one-shot (background title/topic
  // generation) NEVER reaches this session at all: the Go executor's isComposerUtilityOneShot diverts it to a
  // distinct EPHEMERAL sessionId (mintComposerSessionID) BEFORE the bridge sees it, so it lands in a separate
  // Session and can never cancel this real stream. (Verified: deriveSessionID in cursor_composer_clienttools.go.)
  //
  // ADD-37 vs ADD-36 — WHY cancel-and-REPLACE (not synthesize-cancellations-then-RESUME) for sub-case (a):
  // ADD-36 (the concurrent path above, gated on `session.activeRes`) synthesizes MODEL-VISIBLE cancellations for
  // the unanswered pendings so the EXISTING live run RESUMES and consumes user text the executor folded into a
  // matched tool result — it has a run that is mid-stream and a result to ride on. A paused-interrupt has NEITHER:
  // no tool result matched and no streaming run to resume; the new instruction drives a FRESH logical send
  // (enqueueTurn -> runTurn -> driveUserSend). If we instead resolved the old run's pendings here (ADD-36 style),
  // its wait() callback would fire onRunComplete and the OLD run would resume against the OLD context, then
  // COLLIDE with the fresh turn enqueueTurn is about to drive on the same durable agent — exactly the ADD-90
  // double-send race the cancel-then-enqueue ordering exists to prevent. So cancel-and-replace is the correct
  // primitive for a genuine new-user interrupt; a synthesize-then-resume would be WRONG here, not merely redundant.
  // The interrupted (unanswered) tool-call INTENT is NOT lost to the redirected turn: cancel() keeps session.agentId
  // + session.seeded, so the fresh send's ensureAgent resumeAgent()s the DURABLE Cursor agent, which holds the prior
  // assistant tool-call turn server-side (ensureAgent: a successful resume implies prior turns -> seeded, no
  // re-prepend). And the dropped pendings are NOT a false success: cancel() rejectAllPending("session cancelled")
  // fails each awaiting tool promise model-visibly — the new turn's terminal is the only clean end_turn/[DONE], and
  // it belongs to the new instruction, never to the superseded tool calls.
  if (session.run) {
    dbg("handleTurn USER INTERRUPT of a live run -> cancel stale run + drive new turn (ADD-37/ADD-106)", sessionId, "streaming=" + !!session.activeRes, "pending=" + session.pending.size, "waiters=" + session.waiters);
    await session.cancel();
  }
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
  const ka = setInterval(() => { try { res.write(formatSseData({ type: "ping" })); } catch {} }, SSE_KEEPALIVE_MS);
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
      try { res.write(formatSseData({ type: "turn_end", stop_reason: "end_turn" })); res.write("data: [DONE]\n\n"); res.end(); } catch {}
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

function refreshSessionFromBody(session, body) {
  if (Array.isArray(body.tools)) {
    session.advertise = dedupeByName(body.tools.map((t) => ({
      name: t.name, toolName: t.name, providerIdentifier: "cc", description: t.description || "",
      inputSchema: augmentUnderspecifiedToolSchema(t.name, t.inputSchema || t.parameters || undefined),
    })));
  }
  if (body.clientEnv && typeof body.clientEnv === "object") session.clientEnv = body.clientEnv;
}

async function runTurn(req, res, session, model, input, constraints = {}) {
  // ADD-98/ADD-101: declare the turn-latch + close handler BEFORE the first res.write and before assigning
  // session.activeRes, then register res.on('close') first and wrap the whole body (from the activeRes
  // assignment onward) in try/finally. A throw on the INITIAL write (a socket already destroyed before/at the
  // first write) must not strand session.activeRes set or leak the keepalive — the finally always clears them.
  let settled = false;
  let resolveTurn;
  const turnSettled = new Promise((resolve) => { resolveTurn = resolve; });
  const settleOnce = () => { if (!settled) { settled = true; resolveTurn(); } };
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
  let keepalive = null;
  try {
    session.activeRes = res; session.touch(); session.turnToken++;
    session.writeFailed = false; // ADD-100: a fresh turn starts with a healthy write path
    // H08: keep the dispatch seam's native-tool gating current for THIS turn even on a continuation that does
    // not call driveUserSend (the model may emit new native tool calls while resuming). driveUserSend sets it
    // again on a send; this top-level set covers the resume-only path too.
    session.toolChoice = (constraints && constraints.toolChoice) || "";
    session.settleTurn = settleOnce;
    // ADD-98: register the disconnect handler BEFORE the first write so a close during/just-before the initial
    // write is observed and the finally still clears activeRes.
    res.on("close", onClose);
    res.write(formatSseData({ type: "session", sessionId: session.id }));
    // #14: deliver any text/reasoning the run produced BETWEEN turns (while no response was open), in order,
    // before this turn's own new output — so a resuming turn never starts on a gap of dropped catch-up text.
    session.flushPendingDeltas();

    // Typed keepalive (NOT a ": keepalive" comment — the Go executor forwards only "data: " lines, so a
    // comment never reaches the client). The executor renders {"type":"ping"} into the inbound schema's
    // keepalive frame, resetting the client's idle watchdog during long/quiet turns.
    keepalive = setInterval(() => { try { res.write(formatSseData({ type: "ping" })); } catch {} }, SSE_KEEPALIVE_MS);
    if (keepalive.unref) keepalive.unref();

  // driveUserSend performs a model-visible user send on the EXISTING no-timeout agent: it seeds (prepends
  // system + prior history on the FIRST send for the session), applies the enforced constraint instructions,
  // gates the advertised tools by tool_choice, attaches any images, and wires the run's completion callback
  // bound to this run's epoch (mirrors the new-user seed path). It is the single send path shared by the
  // new-user turn, the C1 mixed-turn fresh-send, and the C2/C3 re-seed — so they never drift. extraImages
  // (BR9) are merged in addition to input.images (e.g. images carried inside tool results).
  const driveUserSend = async (userText, extraImages) => {
    userText = appendRulesReminder(userText); // CURSOR_COMPOSER_USER_MSG_REMINDER: standing per-turn nudge (off by default)
    session.streamedText = "";   // reset per user turn (NOT across tool-result continuations within a run)
    session.reasonedThisRun = false; // #15: mirror streamedText — reasoning-produced tracking spans this run
    session.resetLoopBounds();   // ADD-106: a fresh send begins a new logical run -> reset the agentic-loop counters
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
    // Tool-awareness hardening (prompt mode only — the "rule" mode delivers this via requestContext.rules instead).
    // Append a manifest of EVERY offered tool (tool_choice-effective set) so composer treats the client's
    // advertised/MCP tools as first-class and CALLS them instead of self-executing. Client-agnostic.
    if (TOOL_MANIFEST_IN_PROMPT) {
      const manifest = toolManifest(effectiveAdvertise(session.advertise, turnToolChoice));
      if (manifest) { text = text ? text + "\n\n" + manifest : manifest; dbg("toolManifest injected (prompt)", "session=" + session.id, "bytes=" + manifest.length); }
    }
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
    const effAdv = effectiveAdvertise(session.advertise, turnToolChoice);
    session.advertise = effAdv;
    // ADD-40: pin the turn-scoped effective set for the LIFETIME of this run. `advertise` is restored to the
    // full set in the finally (needed for cross-turn refresh + re-seed), but the model's ASYNC MCP dispatch
    // later in the run reads activeAdvertise via advertiseForGating(), so a tool_choice-disallowed tool can no
    // longer be reconciled/dispatched from the restored full inventory. Cleared on run complete/error/cancel.
    session.activeAdvertise = effAdv;
    dbg("runTurn -> agent.send", "session=" + session.id, "seeded(after)=" + session.seeded,
      "msgTextLen=" + (typeof msg === "string" ? msg.length : (msg.text || "").length),
      "images=" + allImages.length, "effAdvertise=" + session.advertise.length, "model=" + model);
    const ep = ++session.runEpoch; // this run's epoch; its completion callback must ignore a result if cancel() (or a later run) advanced it
    await als.run({ session }, async () => {
      try {
        session.run = await agent.send(msg, streamCallbacks(session, ep));
      } catch (sendErr) {
        // ADD-115: a RESUMED durable agent can be wedged on a PRIOR run that died abnormally (e.g. a
        // mid-tool-call drop + bridge restart): the SDK refuses the new send with "already has active run".
        // Expire that stuck run via SendOptions.local.force and retry ONCE so the conversation recovers IN PLACE
        // (durable context intact) instead of erroring out / forcing a context-light fork+reseed. Scoped to that
        // one error; any other send failure rolls the seed flags back (H11) and propagates unchanged.
        const stuckRun = /already has (an )?active run|active run\b/i.test((sendErr && sendErr.message) || "");
        if (stuckRun) {
          try {
            dbg("runTurn agent.send stuck on a prior run -> retry with local.force=true", "session=" + session.id, (sendErr && sendErr.message) || String(sendErr));
            session.run = await agent.send(msg, { ...streamCallbacks(session, ep), local: { force: true } });
          } catch (forceErr) {
            session.seeded = seededBefore;
            session.seededSystem = seededSystemBefore;
            dbg("runTurn force-retry THREW (rolled back seeded)", "session=" + session.id, (forceErr && forceErr.stack) || (forceErr && forceErr.message) || String(forceErr));
            throw forceErr;
          }
        } else {
          // H11: the send failed — roll the seed flags back to their pre-send values so a retry on this same
          // in-memory session re-prepends the system + history prelude (the first send never actually landed).
          session.seeded = seededBefore;
          session.seededSystem = seededSystemBefore;
          dbg("runTurn agent.send THREW (rolled back seeded)", "session=" + session.id, "seeded->" + session.seeded, (sendErr && sendErr.stack) || (sendErr && sendErr.message) || String(sendErr));
          throw sendErr;
        }
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
      // ADD-62: the send under `model` landed and this run owns the session -> record the model the durable
      // agent is now running. A later turn requesting a DIFFERENT model rotates the durable agent (above).
      session.model = model;
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
  // ADD-90: pass {notify:false} so cancel() does NOT release a queued new-user waiter here — that waiter must
  // not be promoted until driveUserSend has installed the replacement session.run (whose wait()->onRunComplete/
  // onRunError fires notifyLogicalDone on real completion). Otherwise the queued turn could start a SECOND
  // concurrent send on the same durable agent in the window between cancel and the replacement send. If the
  // replacement send fails, the runTurn catch path fires notifyLogicalDone as a safety net so the FIFO advances.
  const cancelStaleRun = async () => {
    session.settleTurn = null;
    await session.cancel({ notify: false });
    session.settleTurn = settleOnce;
    session.done = false; // cancel() set done=true; clear it so a subsequent driveUserSend wires completion
  };

    dbg("runTurn START", "session=" + session.id, "inputType=" + input.type, "turnToken=" + session.turnToken);
    // ADD-62: a model change on an ESTABLISHED session. The durable agent was created/resumed under the OLD
    // model; ensureAgent below would resumeAgent it and silently answer from the wrong model. Rotate the
    // durable agent (separate modelEpoch budget) + force a re-seed under the new model. Gate on
    // `session.run === null` for the SAME reason as the fingerprint re-seed: a live/paused run is what a
    // tool_results continuation is answering, and rotating it mid-pause would strand the client's in-flight
    // tool work. A genuine model switch arrives as a fresh turn after the prior run completed (run===null).
    // (A bare resume-only continuation with run===null but a changed model also rotates — correct: the old
    // agent is the wrong model.) `forceModelReseed` then routes through the re-seed path below.
    let forceModelReseed = false;
    if (session.run === null && session.model && session.model !== model) {
      dbg("runTurn MODEL CHANGED (no live run) -> rotate durable agent + re-seed", "session=" + session.id, "from=" + session.model, "to=" + model);
      await session.rotateForModelChange(model);
      forceModelReseed = true;
    }
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

    if (forceReseed || forceModelReseed) {
      // Re-seed path (C2 / ADD-62 model change): drive a fresh user send from the replaced history + system +
      // the trailing user text (userText on a continuation, text on a new-user turn). For a model change the
      // durable agent was already rotated + the run cancelled by rotateForModelChange; the new send seeds the
      // history into the FRESH agent under the new model. `reseeding` keeps ensureAgent's BR-DS probe from
      // re-marking the session seeded (we WANT to prepend the history into the fresh agent).
      session.reseeding = true;
      try {
        const seedText = input.userText || input.text || "";
        await driveUserSend(seedText, collectToolResultImages(input));
      } finally {
        session.reseeding = false;
      }
    } else if (input.type === "tool_results") {
      // C1/BR5: a real trailing user message in this mixed turn (set by the executor only for a genuine user
      // message, never a pure system-reminder). If present AND nothing is left to resume (no pending after
      // resolve, or nothing matched at all), drive a fresh user send so the user's message is answered —
      // instead of an empty end_turn. tool-result images (BR9/EX3) are folded into that send. M28: an
      // image-only trailing message (empty userText but images present) is ALSO user payload and must drive a
      // fresh send, so the model answers about the image instead of an empty turn.
      const hasUserText = typeof input.userText === "string" && input.userText.length > 0;
      // EX3 (partial-batch safe): the tool-result images awaiting delivery THIS logical turn = any stashed from an
      // EARLIER partial batch + this batch's images. A PARTIAL batch (other tools still pending) must not fold its
      // image yet — that would cancel the still-pending tools' in-flight work — so it is stashed and folded only
      // when the batch finally completes (the run would otherwise resume). The NON-image results still reach the
      // model via their own /mcp tools/call responses, so only the image has to wait for the batch to finish.
      const stashedToolResultImages = Array.isArray(session.stashedToolResultImages) ? session.stashedToolResultImages : [];
      // EX3: a base64 tool-result image rides its OWN dispatchMcp result as McpImageContent (delivered at
      // resolvePending), so it is NOT a fresh-send payload when the clean path is on — let the run resume. url-form
      // images can't be McpImageContent (no base64 `data`), so they ALWAYS fall back to the fresh-send fold here
      // (this batch + any earlier partial-batch stash). When the clean path is off, ALL images fold.
      const allTurnToolResultImages = stashedToolResultImages.concat(collectToolResultImages(input));
      const turnToolResultImages = mcpImageResultsEnabled()
        ? allTurnToolResultImages.filter((im) => !(im && typeof im.data === "string" && im.data))
        : allTurnToolResultImages;
      const hasUserImages = (Array.isArray(input.images) && input.images.length > 0) || turnToolResultImages.length > 0;
      const hasUserPayload = hasUserText || hasUserImages;
      // ADD-89 (bridge half): pre-scan — BEFORE matchToolResults mutates `pending` — whether any result that
      // WILL resolve a pending carries isError. A reply built on a FAILED tool must not silently resume the
      // live run (the user's trailing text would be answered as if the tool succeeded). When there is trailing
      // user payload AND an answered result failed, we FORCE the C1 fresh-send path even if the run would
      // otherwise resume, so the failure reaches the model AS failure and the user text is a real user turn.
      const answeredError = (input.results || []).some((tr) =>
        tr && tr.isError === true && (session.pending.has(tr.toolCallId) || (tr.idless === true && session.pending.size === 1)));
      const forceFreshOnError = hasUserPayload && answeredError; // ADD-89
      // EX3 (warm half): a tool result carrying an IMAGE cannot ride the text-only Cursor tool-result protobuf
      // into a resuming run, so resuming would silently drop the image — the model never sees the screenshot and
      // re-reads the file (the reported "can't read photos from a file" bug). Force the C1 fresh-send (which folds
      // turnToolResultImages) instead of resuming, exactly as forceFreshOnError does for a failed tool. The
      // cold/reseed twin is composerReseedLostContinuation (executor). noneToResume stays the C1 gate below, so a
      // PARTIAL batch never cancels its still-pending tools — its image waits in the stash until the batch
      // completes (the run would resume), at which point the whole stash is folded.
      const forceFreshOnImage = turnToolResultImages.length > 0;
      // The run will resume and stream the model's answer ONLY when this continuation answers the LAST pending
      // tool(s) and a run is still live. Pre-compute that here (still BEFORE matching) so ADD-77/ADD-83 can
      // inject a changed-system / per-turn-constraint preamble into the LAST tool result content BEFORE it is
      // resolved into the resuming run. `willResolveAllPending` = every currently-pending id is answered by this
      // batch. Matched/idless results that hit a pending count toward clearing it.
      let answeredPendingCount = 0;
      for (const tr of input.results || []) {
        if (tr && session.pending.has(tr.toolCallId)) answeredPendingCount++;
        else if (tr && tr.idless === true && session.pending.size === 1) answeredPendingCount++;
      }
      const willResolveAllPending = session.pending.size > 0 && answeredPendingCount >= session.pending.size;
      const willResume = willResolveAllPending && session.run !== null && !forceFreshOnError && !forceFreshOnImage;
      // ADD-77 + ADD-83 (bridge half): when this continuation will RESUME the live run, the resumed run answers
      // under the system + constraints the run was STARTED with — but the client may have changed its system
      // prompt (e.g. ExitPlanMode) or per-turn constraints (response_format/stop/max_tokens/tool_choice) on THIS
      // request. The SDK cannot mutate a paused run's instructions, so inject a synthetic, model-visible preamble
      // into the LAST tool result content (the last thing the model reads before replying) and update
      // seededSystem so a later send does not re-apply it. Stable marker strings (tested). We do NOT inject on
      // the forced-fresh-on-error path (driveUserSend applies system+constraints there) to avoid double-applying.
      if (willResume && Array.isArray(input.results) && input.results.length) {
        const preamble = [];
        if (input.system && input.system !== session.seededSystem) {
          preamble.push("[Updated system instructions:]\n" + input.system);
          session.seededSystem = input.system; // remember so a later send does not re-apply the same swap
        }
        const ci = constraintInstructions({ ...constraints });
        if (ci) preamble.push("[Constraints for your reply:]\n" + ci);
        if (preamble.length) {
          const last = input.results[input.results.length - 1];
          const prev = typeof last.content === "string" ? last.content : (last.content == null ? "" : safeJson(last.content));
          last.content = preamble.join("\n\n") + (prev ? "\n\n" + prev : "");
          dbg("runTurn tool_results resume -> inject system/constraints preamble (ADD-77/ADD-83)", "session=" + session.id, "systemSwap=" + (input.system && input.system !== undefined), "constraints=" + (ci ? 1 : 0));
        }
      }
      // C-TOOLRESULT-MATCH: match the batch with the ONE shared strict matcher (resolve-by-id -> benign-ack if
      // ever-emitted -> else unknown). C03: the old inline `pending.size===1` fallback is GONE (it lived here);
      // matchToolResults keeps only an explicit `tr.idless`-marked lone-pending escape. Comment 2 idempotency
      // (re-sent already-resolved ids are benign, not fatal) is preserved by the everEmitted/delivered checks.
      const { matched, unknown } = session.matchToolResults(input.results);
      dbg("runTurn tool_results", "session=" + session.id, "matched=" + matched, "of=" + ((input.results || []).length),
        "pending=" + session.pending.size, "undelivered=" + session.undelivered.length, "unknown=" + safeJson(unknown), "forceFreshOnError=" + forceFreshOnError);
      // The run will resume and stream the model's answer ONLY when this continuation answered tool(s) and
      // nothing is left pending and the run is still live AND we are not forcing a fresh send on a failed tool.
      // In THAT case the trailing user message rode along folded into the last tool result (executor C1
      // belt-and-suspenders), so a separate fresh send would be redundant AND would cancel the resuming run.
      const runWillResume = matched > 0 && session.pending.size === 0 && session.run !== null && !forceFreshOnError && !forceFreshOnImage;
      const noneToResume = matched === 0 || session.pending.size === 0;
      // DECISION ORDER (C-TOOLRESULT-MATCH; H06 puts unknown BEFORE the partial-pending clean ack):
      //   1. C1 fresh-send (user payload + nothing will resume, OR ADD-89 forced fresh on a failed tool)
      //   2. unknown.length>0 -> ERROR turn (a foreign id must never hide behind a partial clean ack)
      //   3. flushUndelivered (late tools delivered this turn)
      //   4. pending.size>0 -> benign empty end_turn (true incremental answer; run stays paused)
      //   5. matched===0 && a paused run DIED upstream -> ERROR turn (BR2; never fake success)
      //   6. else -> benign end_turn (idempotent stale/duplicate ack)
      if (hasUserPayload && (noneToResume || forceFreshOnError) && !runWillResume) {
        // The user's trailing message/image would otherwise produce an empty end_turn (no output is coming).
        // Answer it with a fresh send. If a run is still live (e.g. matched===0 but unrelated tools are
        // pending), the user is redirecting — cancel the stale run first so we don't spawn a concurrent /
        // orphaned run, then send. cancel() nulls the agent + bumps epoch; driveUserSend re-resumes a live agent.
        dbg("runTurn tool_results -> C1 fresh user send", "session=" + session.id, "matched=" + matched, "pending=" + session.pending.size, "runLive=" + (session.run !== null), "text=" + hasUserText, "images=" + hasUserImages);
        if (session.run !== null) { await cancelStaleRun(); session.seeded = true; }
        // M28: send "" for a user-pasted image-only turn (driveUserSend folds input.images in). EX3: for a
        // tool-result image with no trailing user text, mark the image's provenance with a NON-directive
        // parenthetical — NOT an instruction. The user's real task lives in the durable context (or userText); a
        // "read it and continue"-style directive here overrides it (observed: the model abandoned the task to
        // "continue" with a self-invented one). Folds the whole stash (this batch + any earlier partial batches).
        const freshText = hasUserText ? input.userText
          : (turnToolResultImages.length > 0 ? "(The attached image is the output of a tool call you made.)" : "");
        session.stashedToolResultImages = []; // folding now — clear the partial-batch stash
        await driveUserSend(freshText, turnToolResultImages);
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
        // EX3: a tool-result image in THIS partial batch cannot be folded now (that would cancel the still-
        // pending tools), so carry it in the stash — it is folded when the batch completes and the run resumes.
        session.stashedToolResultImages = turnToolResultImages;
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
    // ADD-101: ALWAYS settle the turn on an error so the per-turn latch (turnSettled) resolves — otherwise a
    // throw before any settle() call leaves the latch unresolved (queue/lifecycle stalls). settle() is idempotent.
    session.settle();
    // ADD-90 safety net (guarded): if this throw aborted a C1/reseed replacement send AFTER cancelStaleRun
    // (which used notify:false) and NO run was installed, nothing will fire onRunComplete to release a queued
    // waiter -> release it here so the FIFO does not hang. But ONLY when no run is live: if a run WAS installed,
    // its wait()->onRunComplete/onRunError will notify on REAL completion, and releasing a waiter early here
    // would re-introduce the very ADD-90 race (the queued turn starting concurrently with the live run).
    if (session.run === null) session.notifyLogicalDone();
    // ADD-61: an EMPTY newly-created session whose first turn failed before any AGENT was even established
    // (getPlatform/ensureAgent threw — a transient platform-init failure, or the ADD-53 key-collision reject)
    // must NOT linger in the sessions map. While present it pins its platform (platformHasSession) and blocks
    // idle eviction. Delete it ONLY when it is truly empty AND no agent was created: never seeded, no agent,
    // no live/paused run, no pending tools, no queued waiters. The `session.agent === null` guard is the line
    // between this case and H11 (agent.send failed AFTER ensureAgent succeeded -> session.agent IS set): H11
    // deliberately KEEPS the session so the retry reuses the cached agent and re-prepends system + history.
    if (!session.seeded && session.agent === null && session.run === null && session.pending.size === 0 && !session.hasQueuedWaiters() && sessions.get(session.id) === session) {
      dbg("runTurn drop empty failed-first-turn session (ADD-61)", "session=" + session.id);
      sessions.delete(session.id);
      void session.cancel();
    }
  } finally {
    if (keepalive) clearInterval(keepalive);
    res.off("close", onClose);
    if (session.activeRes === res) session.activeRes = null;
    // ADD-100: stop buffering output + detach the 'drain' listener for THIS response (the turn is over).
    session.detachDrain(); session.outQueue = []; session.outQueueBytes = 0;
    // ADD-101: clear the per-turn latch handle when it still points at THIS turn's settleOnce, so a LATER
    // cancellation (e.g. from a queued turn) never fires a stale settleTurn belonging to an already-finished turn.
    if (session.settleTurn === settleOnce) session.settleTurn = null;
    try { res.write("data: [DONE]\n\n"); res.end(); } catch {}
  }
}

const server = createServer(async (req, res) => {
  if (req.method === "OPTIONS") { res.setHeader("Access-Control-Allow-Origin", "*"); res.writeHead(204); res.end(); return; }
  if (req.method === "GET" && req.url === "/health") {
    res.setHeader("Access-Control-Allow-Origin", "*");
    res.writeHead(200, { "Content-Type": "application/json" });
    // ADD-58: only expose the patched flag + live session count to a LOOPBACK caller or one presenting the
    // bridge token. A remote/forwarded caller (reverse proxy, port-forward) gets a bare {ok:true} liveness —
    // it must not learn whether the bridge is patched or how many sessions are active.
    res.end(JSON.stringify(healthBody(req)));
    return;
  }
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

// CRASH BACKSTOP (P0): the @cursor/sdk validates advertised tool schemas with ajv in STRICT mode and can THROW
// from INSIDE its own async callbacks (e.g. a malformed inputSchema reaching tools/list) — a path the bridge
// cannot wrap per-seam because it lives inside the SDK. An uncaught throw there would KILL this Node process and
// take the whole bridge down, so the Go executor then 503s every session until a manual redeploy (the schema-
// compaction incident). Keep the process ALIVE and log LOUDLY — full stack, always, never silent, so a real bug
// stays visible rather than masked. The bridge's OWN seams still surface a typed error turn to the client; a
// single wedged turn is bounded by the per-tool PENDING_TIMEOUT / session-TTL guards, but a dead process is not.
process.on("uncaughtException", (err, origin) => {
  try { console.error("[cursor-agent-bridge] FATAL uncaughtException (process kept alive) origin=" + origin + ":", (err && err.stack) ? err.stack : err); } catch { /* a logger throw must never re-crash the handler */ }
});
// sessionForClosedInputStream attributes a FLOATING WriteIterableClosedError to the one session it can SAFELY
// blame. The error means a run's INPUT pipe (the SDK's WritableIterable — the channel agent.send writes into)
// was torn down by an upstream Cursor stream drop and a late write then hit it; it surfaces as an
// unhandledRejection instead of rejecting run.wait()->onRunError, so the dead run would otherwise linger (the
// client sees a silent "socket closed", pendings stay stranded until PENDING_TIMEOUT, and the session looks
// busy). The reason carries NO run/session handle, so attribute ONLY when EXACTLY ONE session has an in-flight
// STREAMING run (run set, activeRes open, not done) — the unambiguous common case (a single CC user runs one
// turn at a time). 0 or 2+ candidates -> null (refuse to guess; blaming the wrong one would kill a healthy
// concurrent turn — same safe degradation as before). Exported for tests.
function sessionForClosedInputStream(reason, sessionsMap) {
  const closed = reason && (reason.name === "WriteIterableClosedError" ||
    /WritableIterable is closed/i.test((reason && reason.message) || ""));
  return closed ? soleStreamingSession(sessionsMap) : null;
}

process.on("unhandledRejection", (reason) => {
  try {
    const victim = sessionForClosedInputStream(reason, sessions);
    if (victim) {
      // Convert the leaked input-pipe closure into a clean run teardown: the in-flight turn ends with a typed
      // error (not a silent socket close), pendings reject at once (not stranded until PENDING_TIMEOUT), and
      // the session is freed so the next turn routes against clean state. onRunError is idempotent on `done`,
      // so a racing real onRunComplete/onRunError cannot double-tear-down.
      console.error("[cursor-agent-bridge] run input stream closed mid-turn (upstream Cursor drop) -> clean teardown session=" + victim.id + ":", (reason && reason.message) || reason);
      try { victim.onRunError(new Error("upstream Cursor stream closed mid-run; the turn was interrupted")); } catch { /* never throw from the handler */ }
      return;
    }
    if (isUpstreamRateLimit(reason)) {
      // Cursor flood-protected the account (NGHTTP2_ENHANCE_YOUR_CALM): the cached HTTP/2 connection is now
      // poisoned and every reused stream on it fails until it is recycled. Drop + dispose the poisoned platform
      // so the next turn dials fresh, and OPEN the per-key breaker so client retries back off instead of
      // immediately re-poisoning the new connection.
      const kh = rateLimitedKeyToRecycle(sessions, platforms);
      if (kh) {
        recyclePlatform(kh);
        const e = tripBreaker(kh);
        console.error("[cursor-agent-bridge] upstream rate-limit (ENHANCE_YOUR_CALM) -> recycled connection + breaker OPEN key=" + kh + " fails=" + e.fails + " ~" + Math.ceil(breakerRetryAfterMs(kh) / 1000) + "s:", (reason && reason.message) || reason);
      } else {
        console.error("[cursor-agent-bridge] upstream rate-limit (ENHANCE_YOUR_CALM) but could not safely attribute a key (multi-tenant) -> log only:", (reason && reason.message) || reason);
      }
      return;
    }
  } catch { /* never throw from the handler */ }
  try { console.error("[cursor-agent-bridge] unhandledRejection (process kept alive):", (reason && reason.stack) ? reason.stack : reason); } catch { /* never throw from the handler */ }
});

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

// Startup self-test (part 3, RESULT SERIALIZATION seam) — ADD-74. Parts 1+2 prove native DISPATCH is
// intercepted (the model's tool call reaches the bridge instead of the sidecar FS), but they do NOT prove the
// RETURN trip: that a result `__ccJson` we build can be serialized back into the SDK's protobuf result shape
// by the patched serializeResult/serializeStream factory `$` (which does <ExecClientMessage field>.T.fromJson).
// A same-version SDK bundle drift can change the ExecClientMessage field localNames / result message classes
// while the dispatch anchors still match — so the bridge would announce "self-tests passed" yet the FIRST real
// client tool result would fail inside `$` ("unknown result case" / "invalid result shape"), a runtime tool
// failure instead of a fail-fast deploy failure.
//
// CONTRACT (C-ADD74-SERIALIZE-SEAM): the patcher (scripts/apply-clienttools-patch.cjs) exposes the EXACT `$`
// factory it injects at the serializeResult/serializeStream site as `globalThis.__CC_SELFTEST_SERIALIZE`
// (verbatim name — the patcher + scripts/run-selftests.mjs use this exact identifier). It is a function
// `(resultCaseName) => (id, value) => ExecClientMessage` that, when `value` carries `__ccJson`, builds the
// real protobuf result via the SDK's <ExecClientMessage field>.T.fromJson (and THROWS on an unknown case /
// invalid shape, exactly like the live seam). This mirrors the existing __CC_SELFTEST_DISPATCH_U/S harness.
// Here we drive REPRESENTATIVE result payloads — the exact shapes CC_CASES.buildResult/buildChunks produce —
// through it and fail startup if any cannot serialize. (Bridge-side only; the patcher side is a separate file.)
async function selfTestResultSerialization() {
  const factory = globalThis.__CC_SELFTEST_SERIALIZE;
  if (typeof factory !== "function") {
    throw new Error("self-test: patched SDK bundle did not install the result-serialization harness (__CC_SELFTEST_SERIALIZE) — patch missing/stale (ADD-74); refusing to start");
  }
  // EXHAUSTIVE coverage (ADD-74 widened): rather than a hand-picked sample, ENUMERATE every result shape the
  // bridge can emit over the seam — straight from CC_CASES / CONTROL_ALLOW / CONTROL_TYPED / TYPED_UNAVAILABLE_U
  // and the blockedNative/typedUnavailable/MCP/requestContext builders — and drive each through the patched
  // `$`/fromJson. Each is built by the SAME function the live run uses, so the test cannot drift from real
  // traffic, and a new CC_CASES/CONTROL entry is covered automatically. This is the regression that shipped
  // { error: { message } } for the no-such-field agent.v1.ReadError AND deleted_file:true (a bool where the
  // proto wants a string) — both invisible until a real failed/delete tool hit the seam in production.
  //
  // resultCase mapping: the SDK serializes a dispatch case `<x>Args` as result case `<x>Result`
  // (redactedReadArgs -> redactedReadResult, an alias of ReadResult; shellArgs -> shellResult). The streaming
  // shell case is the lone exception: its chunks serialize as the `shellStream` case, not `shellStreamResult`.
  const resultCaseFor = (cas) => cas.replace(/Args$/, "Result");
  const st = { path: "/x", command: "ls", workingDirectory: "/workspace", fileText: "alpha\nbeta", offset: 0, limit: 20, returnFileContentAfterWrite: true };
  const ctx = { cwd: "/workspace" };
  const checks = []; // { case, label, ccJson }
  const add = (rc, label, ccJson) => checks.push({ case: rc, label, ccJson });

  // 1) Every unary CC_CASES builder: success AND error variant (fs tools build a real failure; shell encodes
  //    failure via exitCode so isError just flips it). grep/ls carry buildResult:null (handled via H17 below).
  for (const [cas, spec] of Object.entries(CC_CASES)) {
    if (spec.stream || typeof spec.buildResult !== "function") continue;
    const rc = resultCaseFor(cas);
    add(rc, cas + ":success", spec.buildResult("alpha\nbeta", st, false, ctx));
    add(rc, cas + ":error", spec.buildResult("the tool failed", st, true, ctx));
  }
  // 2) Shell STREAM chunks (stdout/stderr/exit) serialize as the `shellStream` case — success + aborted/error.
  for (const chunk of CC_CASES.shellStreamArgs.buildChunks("output line\n", false, ctx)) add("shellStream", "shellStream:ok", chunk);
  for (const chunk of CC_CASES.shellStreamArgs.buildChunks("boom", true, ctx)) add("shellStream", "shellStream:err", chunk);
  // 3) blockedNativeResult (H08) for every native case the gate can block.
  for (const cas of ["readArgs", "redactedReadArgs", "writeArgs", "deleteArgs", "shellArgs"]) add(resultCaseFor(cas), "blocked:" + cas, blockedNativeResult(cas, st, "/workspace").__ccJson);
  // 4) CONTROL_ALLOW precheck cases answer { allowlisted: true }.
  for (const cas of Object.keys(CONTROL_ALLOW)) add(resultCaseFor(cas), "allow:" + cas, { allowlisted: true });
  // 5) CONTROL_TYPED proactive cases answer a typed value (e.g. { rejected: {} }).
  for (const [cas, val] of Object.entries(CONTROL_TYPED)) add(resultCaseFor(cas), "typed:" + cas, val);
  // 6) TYPED_UNAVAILABLE_U (H17): model-visible typed unavailable result for each.
  for (const cas of TYPED_UNAVAILABLE_U) add(resultCaseFor(cas), "unavailable:" + cas, typedUnavailableResult(cas).__ccJson);
  // 7) MCP dispatch wrap (handleMcp/mcpDispatch + the "tool not advertised" wrap): drive the SAME builder the
  //    live dispatch uses (mcpDispatchResult), not a hand-retyped literal, so the McpResult shape cannot drift
  //    from real traffic (ADD-74). Cover success isError false/true AND object content (JSON.stringify path).
  add("mcpResult", "mcp:ok", mcpDispatchResult("ok", false));
  add("mcpResult", "mcp:isError", mcpDispatchResult("tool failed", true));
  add("mcpResult", "mcp:object", mcpDispatchResult({ k: "v" }, false));
  // EX3: validate the McpImageContent variant ({image:{data:<base64>,mimeType}}) serializes through the REAL
  // proto at startup, so a wrong shape fails fast here (fail-closed) instead of crashing on the first real image.
  add("mcpResult", "mcp:image", mcpDispatchResult("here is the image", false, [{ data: "iVBORw0KGgo=", mimeType: "image/png" }]));
  // 8) Headless request context (the SDK's first exec on every run).
  add("requestContextResult", "requestContext", headlessRequestContext(null).__ccJson);
  // Validate the always-apply agent.v1.CursorRule proto serializes (independent of TOOL_MANIFEST_MODE), so any
  // future drift in the rule shape fails fast at boot rather than 500ing the first turn that emits a manifest rule.
  add("requestContextResult", "requestContext", requestContextEnvelope({}, ["/workspace"], "/workspace", [toolManifestRule([{ name: "Read", description: "read a file" }], "/workspace")]).__ccJson);
  // Headless MCP state (the runtime's mcp_state_exec reply): McpStateExecResult.success with one connected
  // server carrying one tool proves the success oneof + nested McpStateServer/McpToolDefinition/Value
  // serialize through fromJson — previously only ever exercised as the error variant via TYPED_UNAVAILABLE_U.
  // Also cover the empty (no-advertise) fail-safe so { servers: [] } stays serializable.
  {
    const mcpStateSession = new Session("selftest-mcpstate");
    mcpStateSession.advertise = [{ name: "mcp__nanobanana__generate_image", description: "gen", inputSchema: { type: "object" } }];
    add("mcpStateExecResult", "mcpState:success", headlessMcpState(mcpStateSession).__ccJson);
    add("mcpStateExecResult", "mcpState:empty", headlessMcpState(new Session("selftest-mcpstate-empty")).__ccJson);
    // ERROR variant of the SAME result case: headlessMcpState's catch-fallback emits
    // typedUnavailableResult("mcpStateExecArgs") -> McpStateExecResult.error (McpStateError{error:<string>}).
    // mcpStateExecArgs was removed from TYPED_UNAVAILABLE_U (check #6) when mcp_state switched to McpStateSuccess,
    // so nothing else enumerates this error shape; enumerate it explicitly so the seam validates the shape the
    // live catch still emits (mcpToolsForServer/buildMcpServers can throw on malformed advertise). Without this,
    // a wrong error shape would throw inside fromJson as a runtime tool failure, not a fail-fast deploy error.
    add("mcpStateExecResult", "mcpState:error", typedUnavailableResult("mcpStateExecArgs").__ccJson);
  }

  for (const c of checks) {
    let build;
    try { build = factory(c.case); } catch (e) {
      throw new Error(`self-test: result-serialization factory threw constructing case '${c.case}' (${c.label}): ${(e && e.message) || e} — refusing to start`);
    }
    if (typeof build !== "function") {
      throw new Error(`self-test: result-serialization factory did not return a builder for case '${c.case}' (${c.label}) — refusing to start`);
    }
    try {
      const out = build("selftest-serialize", { __ccJson: c.ccJson });
      if (out == null) throw new Error("builder returned null");
    } catch (e) {
      // This is EXACTLY the failure a real tool result of this kind would hit. Fail fast at startup instead.
      throw new Error(`self-test: result '${c.case}' (${c.label}) could not serialize through the patched fromJson seam (ADD-74): ${(e && e.message) || e} — the sidecar result mapping is out of sync with the SDK; refusing to start`);
    }
  }
  dbg("selfTestResultSerialization passed", "checks=" + checks.length);
}

if (RUN_AS_MAIN) {
  // Single-tenant needs CURSOR_API_KEY; multi-tenant needs CURSOR_AGENT_BRIDGE_TOKEN (per-user keys arrive
  // on each request). Require at least one so the bridge always has a way to obtain a Cursor credential.
  if (!API_KEY && !BRIDGE_TOKEN) { console.error("[bridge] set CURSOR_API_KEY (single-tenant) or CURSOR_AGENT_BRIDGE_TOKEN (multi-tenant) — refusing to start"); process.exit(1); }
  // ADD-105: validate the bind host BEFORE doing any work. A non-loopback bind without multi-tenant auth is
  // refused (fail-closed) unless explicitly overridden; a non-loopback bind always warns about plaintext exposure.
  const bindCheck = validateBindHost(BRIDGE_HOST, MULTI_TENANT, ALLOW_INSECURE_BIND);
  if (!bindCheck.ok) { console.error("[bridge]", bindCheck.error); process.exit(1); }
  if (bindCheck.warn) console.warn("[bridge]", bindCheck.warn);
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
    .then(() => selfTestResultSerialization()) // ADD-74: prove the RETURN trip (result __ccJson -> protobuf via fromJson)
    .then(() => server.listen(PORT, BRIDGE_HOST, () => console.log(`[cursor-agent-bridge] listening on http://${BRIDGE_HOST}:${PORT} (patched CJS, fail-closed, native-unreachable + bundle-seam + result-serialization self-tests passed, durable stateRoot=${STATE_ROOT})`)))
    .catch((e) => { console.error("[bridge]", (e && e.message) || e); process.exit(1); });
}

export { CC_CASES, composerModelSelection, headlessRequestContext, headlessMcpState, Session, reconcileExport, toSdkImages, constraintInstructions, effectiveAdvertise, forcedToolUnavailable, nativeToolBlockedByChoice, toolManifest, toolManifestRule, blockedNativeResult, typedUnavailableResult, mcpDispatchResult, TYPED_UNAVAILABLE_U, parseShellContent, streamCallbacks, ccToolId, authorizeRequest, authorizeRequestWith, platformHasSession, keyHash, loadSdk, selfTestNativeUnreachable, selfTestBundleSeam, selfTestResultSerialization, handleTurn, sessions, sessionForClosedInputStream, isUpstreamRateLimit, recyclePlatform, tripBreaker, breakerOpen, breakerRetryAfterMs, closeBreaker, breakerBackoffMs, soleStreamingSession, rateLimitedKeyToRecycle, upstreamBreaker, platforms, collectToolResultImages, isConversationTooLong, ensureAgent, buildMcpServers, mcpServerKeyForTool, mcpToolsForServer, mcpDispatch, handleMcp, MCP_GROUPING, MCP_SHIM_ENABLED, readBodyBounded, PayloadTooLargeError, MAX_AGENT_TURN_BYTES, envInt, BoundedIdSet, composerWorkspaceCwd, buildReadSuccess, buildWriteSuccess, healthBody, isLoopbackRemote, getPlatform, keyFingerprint, PlatformKeyCollisionError, MAX_SESSIONS, MAX_PLATFORMS, wrapToolInput, truncateLiveToolResult, validateBindHost, resolveBridgeHost, bindHostIsLoopback, COMPOSER_LIVE_TOOL_RESULT_MAX_BYTES, COMPOSER_SCHEMA_INLINE_MAX_BYTES, COMPOSER_OUT_QUEUE_MAX_BYTES, COMPOSER_MAX_TOOL_ROUNDS, COMPOSER_MAX_REPEAT_TOOL, augmentUnderspecifiedToolSchema, normalizeToolArgsToSchema, extractScalarFromWrapper, argContractFor, augmentToolDescription, augmentWorkflowResultOnFailure, augmentBackgroundLaunchResult, snapWorkflowAgentTypes, appendRulesReminder };
function reconcileExport(advertise, want) { const s = new Session("x"); s.advertise = advertise; return s.reconcileToolName(want); }
