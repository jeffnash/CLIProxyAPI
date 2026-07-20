#!/usr/bin/env node
// Cursor Agent Bridge (Cursor Composer Client-Tools) — the official @cursor/sdk drives the Cursor agent, but EVERY tool
// executes in the calling harness on the end user's machine (through CLIProxy), and the sidecar filesystem is
// never touched for tool execution. Any compatible client can own the harness boundary.
//
// TOPOLOGY (the sidecar ONLY talks to CLIProxy, never to the client directly):
//   Harness <-OpenAI/Anthropic-> CLIProxy (Go) <-HTTP/SSE /agent/turn + /agent/continue-> THIS sidecar <-@cursor/sdk-> Cursor API
//
// Tools route to the client via the patched bundle's legacy-named globalThis.__CC_EXEC_U/__CC_EXEC_S ABI; the client's tools[]
// are advertised to the model as mcp_tools via globalThis.__CC_GET_ADVERTISE__ (patch inject). Results
// are built as Cursor protobuf messages by the patched serializeResult ($) doing <Type>.fromJson(ccJson).
//
// Durable client-tool ownership lives in one bridge ToolRound. The bridge
// journals signed wire ids, transport handoff, canonical result receipts,
// callback application, recovery, and terminal state before exposing success.
// Session retains only the live SDK agent/run and non-tool conversation state.
//
// Env: CURSOR_API_KEY (required), CURSOR_AGENT_BRIDGE_PORT (default 9798),
//      CURSOR_AGENT_STATE_ROOT (default ./.cursor-agent-store — a writable volume on Railway),
//      CURSOR_AGENT_PENDING_TIMEOUT_MS (default 600000 in-process abandonment watchdog; NOT an upstream deadline),
//      CURSOR_AGENT_SESSION_TTL_MS (default 1800000 idle session eviction),
//      CURSOR_AGENT_CANCEL_CLEANUP_MS (default 5000; bounds already-requested SDK teardown, never live data),
//      CURSOR_COMPOSER_AGENT_GC (default ON; archive -> quarantine -> delete stale unreferenced SDK agents),
//      CURSOR_COMPOSER_MCP_SHIM (default ON; "0"/"false" disables registering the client's tools via the SDK's
//        official mcpServers path — the in-bridge /mcp streamable-http server),
//      CURSOR_COMPOSER_MCP_GROUPING (one|natural|per-tool, default natural — how advertised tools are
//        partitioned across the hosted MCP servers).

import { createServer } from "node:http";
import { randomUUID, timingSafeEqual, createHash } from "node:crypto";
import { fileURLToPath } from "node:url";
import { createRequire } from "node:module";
import { AsyncLocalStorage } from "node:async_hooks";
import {
  accessSync,
  closeSync,
  constants,
  fsyncSync,
  linkSync,
  mkdirSync,
  openSync,
  readdirSync,
  readFileSync,
  renameSync,
  rmSync,
  statSync,
  statfsSync,
  writeFileSync,
  writeSync,
} from "node:fs";
import path from "node:path";
import {
  ToolRound,
  ToolRoundError,
  canonicalJSONString,
  canonicalResult,
  RoundState,
  CallState,
  DeferredInputState,
  ROUTING_TOKEN_VERSION,
  TerminalReason,
  createRoundInfrastructure,
  probeStateRoot,
} from "./tool-round.mjs";
import { SseWriter } from "./sse-writer.mjs";
import { collectSdkAgents } from "./sdk-agent-gc.mjs";
import { DurableCASConflict, DurableJSONCAS } from "./durable-json-cas.mjs";
import {
  TURN_RECEIPT_VERSION,
  ACCEPTANCE_PHASE,
  LEGACY_FRESH_ATTEMPT_STATE,
  IllegalAcceptanceTransition,
  EnvelopeMutationError,
  resolveAcceptancePhase,
  migrateLegacyAcceptancePhase,
  applyAcceptanceTransition,
  isAcceptancePhase,
  isLegalAcceptanceTransition,
  assertLegalAcceptanceTransition,
  isUnresolvedAcceptanceRecord,
  isUnresolvedAcceptancePhase,
  sendBoundaryCrossed,
  recoveryActionForPhase,
  sameFrozenEnvelope,
  hasPositiveAcceptanceEvidence,
} from "./acceptance-receipt.mjs";
import {
  createStreamJournalFromEnv,
  formatSseFrame,
  hasStreamResumeCapability,
} from "./stream-journal.mjs";
import {
  AdaptiveMemoryBudget,
  DEFAULT_ADAPTIVE_MEMORY_INITIAL_BYTES,
} from "./adaptive-reservation.mjs";




import {
  ToolContractRegistry,
  ToolContractNormalizationError,
  normalizeToolArguments,
  normalizeToolResultEnvelope,
  clientToolFamily,
  resolveClientToolName,
} from "./tool-contract-adapter.mjs";

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
const RAILWAY_RUNTIME = !!(process.env.RAILWAY_ENVIRONMENT || process.env.RAILWAY_PROJECT_ID || process.env.RAILWAY_SERVICE_ID);
const RAILWAY_VOLUME_ROOT = process.env.RAILWAY_VOLUME_MOUNT_PATH ? path.resolve(process.env.RAILWAY_VOLUME_MOUNT_PATH) : "";
const STATE_ROOT_RESOLVED = path.resolve(STATE_ROOT);
const STATE_ROOT_ON_RAILWAY_VOLUME = !!RAILWAY_VOLUME_ROOT && (() => {
  const relative = path.relative(RAILWAY_VOLUME_ROOT, STATE_ROOT_RESOLVED);
  return relative === "" || (!relative.startsWith("..") && !path.isAbsolute(relative));
})();
const EMPTY_CWD = path.join(STATE_ROOT, ".empty");
// Bump only when the durable SDK request-context contract changes incompatibly.
// This is deliberately independent of workspace identity: it prevents old
// agents seeded with proxy-side sentinel paths or the old generic MCP meta-tool
// inventory from surviving the redesign without making cwd/workspace part of
// routing.
const DURABLE_AGENT_CONTEXT_EPOCH = "ct5";
const CLIENT_TOOL_CONTRACT_VERSION = 2;
const INTERNAL_ATTEMPT_MAX_BYTES = 4 << 20;
// PENDING_TIMEOUT_MS / SESSION_TTL_MS must be POSITIVE (a 0 watchdog would reap a tool the instant it is
// emitted; a 0 TTL would evict a session immediately) — floor them at 1ms via min:1.
const PENDING_TIMEOUT_MS = envInt("CURSOR_AGENT_PENDING_TIMEOUT_MS", 600000, { min: 1 });
const SESSION_TTL_MS = envInt("CURSOR_AGENT_SESSION_TTL_MS", 1800000, { min: 1 });
const CANCEL_CLEANUP_MS = envInt("CURSOR_AGENT_CANCEL_CLEANUP_MS", 5000, { min: 100, max: 60000 });
const MAX_SESSIONS = envInt("CURSOR_AGENT_MAX_SESSIONS", 1000, { min: 1 });
const TERMINAL_ROUND_TTL_MS = envInt("CURSOR_AGENT_TERMINAL_ROUND_TTL_MS", 7 * 24 * 60 * 60 * 1000, { min: 0 });
const TERMINAL_ROUND_MAX = envInt("CURSOR_AGENT_TERMINAL_ROUND_MAX", 10000, { min: 0 });
const DURABLE_MAINTENANCE_MS = envInt("CURSOR_AGENT_DURABLE_MAINTENANCE_MS", 5 * 60 * 1000, { min: 10_000 });
const SSE_KEEPALIVE_MS = 15000;
// Tool-batch coalescing window. The @cursor/sdk never pauses for tools — it streams tool calls in waves —
// so this debounce merges a wave (emits <window apart) into ONE turn_end. It is a pure latency<->round-trip
// knob, NOT a correctness control: tools emitted after a turn closes are buffered + re-delivered next turn
// regardless (see emitToolUse/flushJournaledCalls), so any value is safe. Raise it (e.g. 150-200) to coalesce
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
// The advertised registry is structural and fail-closed: duplicate or malformed descriptors abort the turn.
// The /mcp route is dialed by the in-process SDK runtime over loopback only.
const MCP_SHIM_RAW = String(process.env.CURSOR_COMPOSER_MCP_SHIM ?? "").trim().toLowerCase();
const MCP_SHIM_ENABLED = !(MCP_SHIM_RAW === "0" || MCP_SHIM_RAW === "false");
// The pinned SDK can serialize McpImageContent, but live Cursor model routes do not all consume that shape
// reliably: Grok 4.5 has terminated an otherwise valid resumed run with `[unavailable] Error`. Keep inline MCP
// images as an explicit compatibility opt-in only. The default path journals every sibling result and performs
// one faithful fresh send carrying the images after the whole logical tool round is receipted. URL-form images
// always use that recovery path because McpImageContent's base64 `data` field cannot carry them. Read at call
// time (not a load-time const) so tests can exercise both paths.
function mcpImageResultsEnabled() {
  const v = String(process.env.CURSOR_COMPOSER_MCP_IMAGE_RESULTS || "").trim().toLowerCase();
  return v === "1" || v === "true";
}
// These identifiers are intentionally harness-neutral. They are visible to the
// Cursor SDK/model as the provider/server that hosts client-owned tools, so a
// Claude-specific label causes non-Claude harnesses to invent a nonexistent
// "Claude Code MCP" hop. The __CC_* global names remain only as a private ABI
// with the pinned SDK patch; they are never the product-level identity.
const CLIENT_TOOL_PROVIDER_ID = "client-tools";
const DEFAULT_MCP_SERVER_KEY = "client-tools";
// Grouping: how advertised tools are partitioned across MCP servers — one|natural|per-tool (default natural).
//   one      -> a single generic "client-tools" server advertising ALL tools (one MCP connection).
//   natural  -> reconstruct the user's real MCP topology from mcp__<server>__<tool> names; non-mcp tools
//               and harness-flattened MCP names are grouped under the generic "client-tools" server.
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
const replayMemoryBudget = {
  limit: envInt("CURSOR_COMPOSER_REPLAY_GLOBAL_MAX_BYTES", 256 * 1024 * 1024, { min: 1 }),
  used: 0,
};
const REPLAY_MEMORY_INITIAL_BYTES = envInt(
  "CURSOR_COMPOSER_REPLAY_INITIAL_BYTES",
  DEFAULT_ADAPTIVE_MEMORY_INITIAL_BYTES,
  { min: 64 * 1024 },
);

// Shared with Go's bufio.Scanner. The extra MiB covers `data:` framing and
// JSON envelope overhead above the largest accepted request body. If an
// operator raises MAX_AGENT_TURN_BYTES, they must raise this shared contract
// too; accepting bytes that Go cannot read back is a startup configuration
// error, not a runtime 502 surprise.
const MAX_SSE_FRAME_BYTES = envInt("CURSOR_COMPOSER_MAX_SSE_FRAME_BYTES", 65 * 1024 * 1024, { min: 1 << 20 });
if (MAX_SSE_FRAME_BYTES < MAX_AGENT_TURN_BYTES + (1 << 20)) {
  throw new Error("CURSOR_COMPOSER_MAX_SSE_FRAME_BYTES must be at least MAX_AGENT_TURN_BYTES + 1048576 bytes");
}
// Per-response SSE output-queue cap. When a client/proxy applies sustained backpressure, SseWriter queues only
// later frames that have not reached res.write. This bounds the queue so a slow/stuck client cannot OOM the sidecar. Past the
// cap the turn is cancelled with a typed transport error (never a fake success). This is a MEMORY bound, NOT a
// data-path wall-clock timeout (allowed by AGENTS.md); default 8 MB is generous for a quiet/slow client.
const COMPOSER_OUT_QUEUE_MAX_BYTES = envInt("CURSOR_AGENT_OUT_QUEUE_MAX_BYTES", 8 * 1024 * 1024, { min: 1 });
// Cap live tool-result content before it resolves an SDK callback, distinct from the
// executor's history bound. The EXECUTOR's truncateCursorToolResultLive is authoritative and normally wins;
// this is a slightly HIGHER cap so it only fires for content that bypassed the executor cap (a defense-in-depth
// backstop). Both sides stamp the SAME marker substring 'truncated by proxy' so callers and tests agree.
const COMPOSER_LIVE_TOOL_RESULT_MAX_BYTES = envInt("CURSOR_AGENT_LIVE_TOOL_RESULT_MAX_BYTES", 320 * 1024, { min: 1 });
// Cursor's pinned SDK used 40,000 bytes as the threshold for its proxy-local
// agent-tools artifact spill. The SDK patch disables that spill entirely. This
// lower guard threshold identifies a copied large MCP result if the backend
// nevertheless asks a native Write to materialize one under a different path.
const SYNTHETIC_ARTIFACT_RESULT_MIN_BYTES = envInt("CURSOR_AGENT_ARTIFACT_GUARD_MIN_BYTES", 32 * 1024, { min: 1 });
const SYNTHETIC_ARTIFACT_RESULT_WINDOW = 8;
// ADD-103: cap the serialized response_format / json_schema inlined into the prompt as a best-effort
// instruction. A large generated schema (nested $defs, many enums) would otherwise bloat the prompt, leak more
// than necessary, and risk ERROR_CONVERSATION_TOO_LONG — all for a constraint the composer path can only honor
// best-effort anyway (the unsupportedHardGuarantees advisory still tells the model it is non-enforced). Past
// the cap we inline a short note instead of the full schema. A SIZE bound on prompt text, not a timeout.
const COMPOSER_SCHEMA_INLINE_MAX_BYTES = envInt("CURSOR_AGENT_SCHEMA_INLINE_MAX_BYTES", 8 * 1024, { min: 1 });
const STATE_ROOT_MIN_FREE_BYTES = envInt(
  "CURSOR_COMPOSER_STATE_ROOT_MIN_FREE_BYTES",
  64 * 1024 * 1024,
  { min: 1024 * 1024 },
);
const UNRESOLVED_RECEIPT_MAX_BYTES = envInt(
  "CURSOR_COMPOSER_UNRESOLVED_RECEIPT_MAX_BYTES",
  1024 * 1024 * 1024,
  { min: 1024 * 1024 },
);
const UNRESOLVED_RESERVATION_ORPHAN_MS = envInt(
  "CURSOR_COMPOSER_UNRESOLVED_RESERVATION_ORPHAN_MS",
  60 * 60 * 1000,
  { min: 60_000 },
);
const SDK_AGENT_GC_ENABLED = !["0", "false", "off"].includes(
  String(process.env.CURSOR_COMPOSER_AGENT_GC || "1").trim().toLowerCase(),
);
const SDK_AGENT_GC_MIN_IDLE_MS = envInt(
  "CURSOR_COMPOSER_AGENT_GC_MIN_IDLE_MS", 7 * 24 * 60 * 60 * 1000, { min: 60_000 },
);
const SDK_AGENT_GC_QUARANTINE_MS = envInt(
  "CURSOR_COMPOSER_AGENT_GC_QUARANTINE_MS", 24 * 60 * 60 * 1000, { min: 60_000 },
);
const SDK_AGENT_GC_MAX_SCAN = envInt("CURSOR_COMPOSER_AGENT_GC_MAX_SCAN", 10_000, { min: 1 });
const SDK_AGENT_GC_MAX_MUTATIONS = envInt("CURSOR_COMPOSER_AGENT_GC_MAX_MUTATIONS", 50, { min: 1 });
const SDK_AGENT_GC_DIR = path.join(STATE_ROOT, ".cct-agent-gc");

function stateRootDiskStatus(requiredBytes = 0, statfs = statfsSync) {
  try {
    const stats = statfs(STATE_ROOT);
    const freeBytes = Number(stats.bavail) * Number(stats.bsize);
    const required = STATE_ROOT_MIN_FREE_BYTES + Math.max(0, Number(requiredBytes) || 0);
    return {
      ok: Number.isFinite(freeBytes) && freeBytes >= required,
      freeBytes: Number.isFinite(freeBytes) ? freeBytes : 0,
      requiredBytes: required,
    };
  } catch {
    return { ok: false, freeBytes: 0, requiredBytes: STATE_ROOT_MIN_FREE_BYTES };
  }
}
// Shared SSE response headers (unbuffered, so keepalives reach the wire end-to-end).
const SSE_HEADERS = { "Content-Type": "text/event-stream", "Cache-Control": "no-cache", Connection: "keep-alive", "X-Accel-Buffering": "no" };
function formatSseData(obj) { return `data: ${JSON.stringify(obj)}\n\n`; }
function normalizeClientLeaseToken(value) {
  const token = typeof value === "string" ? value.trim() : "";
  return /^[1-9][0-9]{0,19}$/.test(token) ? token : "";
}
function withClientLease(event, source, terminal) {
  const token = normalizeClientLeaseToken(source && source.clientLeaseToken);
  const sessionId = typeof (source && source.sessionId) === "string"
    ? source.sessionId : typeof (source && source.id) === "string" ? source.id : "";
  if (!token || !sessionId) return event;
  return {
    ...event,
    clientLease: { sessionId, token, terminal: terminal === true },
  };
}
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
const SDK_PATCH_DESCRIPTOR_VERSION = 4;
const SDK_PATCHER_VERSION = 5;
const SDK_PRISTINE_BUNDLE_SHA256 = "829ced604bb88908e49fcf5cd31eb22bce4e57d32074b2846d86a6c5afa26881";
const SDK_PRISTINE_INDEX_SHA256 = "3157e86833e5033ce7b870cfd9810edc4b1e9c0637b93170779d6cbb3feba022";
const SDK_PINNED_VERSION = "1.0.23";
const SDK_PATCH_SEAMS = {
  serializer: 1,
  unaryDispatch: 1,
  streamDispatch: 1,
  advertiseRegistry: 1,
  mcpArtifactSpillPolicy: 1,
  mcpMetaToolPolicy: 1,
  localExecutorLoader: 1,
  localSendRequestId: 1,
  moduleExport: 1,
};

// Marker text is only a diagnostic. Runtime trust comes from the descriptor emitted
// by the structural patcher plus exact hashes of both installed CJS files. This
// catches stale caches, partial writes, hand-edited vendor files, and patcher/bridge
// skew before the SDK can execute a tool natively on the sidecar.
function assertPatched(p) {
  if (!p.endsWith(path.join("dist", "cjs", "index.js"))) {
    throw new Error(`[bridge] @cursor/sdk resolved to ${p}, expected dist/cjs/index.js — refusing to start (tools would run natively on the sidecar FS).`);
  }
  const sdkRoot = path.resolve(path.dirname(p), "..", "..");
  const chunk = path.join(sdkRoot, "dist", "cjs", "973.js");
  const descriptorPath = path.join(sdkRoot, ".clienttools-patch-descriptor.json");
  let descriptor;
  let chunkBytes;
  let indexBytes;
  let sdkPackage;
  try {
    descriptor = JSON.parse(readFileSync(descriptorPath, "utf8"));
    chunkBytes = readFileSync(chunk);
    indexBytes = readFileSync(p);
    sdkPackage = JSON.parse(readFileSync(path.join(sdkRoot, "package.json"), "utf8"));
  } catch (e) {
    throw new Error(`[bridge] cannot load the @cursor/sdk structural patch descriptor/files (${(e && e.message) || e}). Run npm ci in sidecars/cursor-bridge. Refusing to start.`);
  }
  const expectedShape = JSON.stringify(SDK_PATCH_SEAMS);
  if (
    sdkPackage.version !== SDK_PINNED_VERSION ||
    descriptor?.sdkVersion !== SDK_PINNED_VERSION ||
    descriptor?.descriptorVersion !== SDK_PATCH_DESCRIPTOR_VERSION ||
    descriptor?.patcherVersion !== SDK_PATCHER_VERSION ||
    descriptor?.bundle !== "dist/cjs/973.js" ||
    descriptor?.index !== "dist/cjs/index.js" ||
    descriptor?.nativeExecutionDefault !== "deny" ||
    descriptor?.mcpArtifactSpillThresholdBytes !== 0 ||
    descriptor?.mcpMetaToolEnabled !== false ||
    descriptor?.localSendIdempotency !== "deterministic-request-id" ||
    descriptor?.sourceVerified !== true ||
    descriptor?.pristineBundleSha256 !== SDK_PRISTINE_BUNDLE_SHA256 ||
    descriptor?.pristineIndexSha256 !== SDK_PRISTINE_INDEX_SHA256 ||
    JSON.stringify(descriptor?.seams) !== expectedShape
  ) {
    throw new Error("[bridge] @cursor/sdk patch descriptor does not match the bridge's exact client-tools contract. Refusing to start.");
  }
  const bundleHash = createHash("sha256").update(chunkBytes).digest("hex");
  const indexHash = createHash("sha256").update(indexBytes).digest("hex");
  if (descriptor.patchedBundleSha256 !== bundleHash || descriptor.patchedIndexSha256 !== indexHash) {
    throw new Error("[bridge] @cursor/sdk installed bytes do not match the patch descriptor. Refusing to start.");
  }
  if (!chunkBytes.subarray(0, 64).toString("latin1").includes("cursor-composer-clienttools-patched-v5") || !indexBytes.toString("latin1").includes("cursor-composer-clienttools-eager-v5")) {
    throw new Error("[bridge] @cursor/sdk descriptor hashes matched but structural markers are absent. Refusing to start.");
  }
  return descriptor;
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

// ccToolId normalizes the SDK's raw correlation id before ToolRound hashes it
// for in-round idempotency. It is never exposed to the harness; ToolRound
// allocates the client-visible signed routing token.
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

// collectToolResultImages gathers compatibility image projections carried inside tool results. Base64 images
// normally travel in the MCP result itself; URL images and recovery paths attach these projections to a
// separate faithful SDK user send. Returns {data,mimeType} or {url[,mimeType,detail]} entries.
function collectToolResultImages(input) {
  const out = [];
  for (const tr of (input && input.results) || []) {
    if (Array.isArray(tr.images)) for (const im of tr.images) if (im) out.push(im);
  }
  return out;
}

// A malformed optional result projection must not strand a tool that already
// executed on the user's machine. Keep signed ID routing strict, but degrade a
// representation-only 422 (bad image envelope, conflicting aliases, etc.) to
// a model-visible error result with the original textual content. The durable
// first receipt remains authoritative on retries.
function accommodateContinuationResults(results) {
  return results.map((raw) => {
    try {
      canonicalResult(raw);
      return raw;
    } catch (error) {
      if (!(error instanceof ToolRoundError) || error.status !== 422) throw error;
      let content = raw && raw.content;
      if (typeof content !== "string") {
        try { content = content === undefined ? "" : JSON.stringify(content); }
        catch { content = ""; }
      }
      const marker = `[proxy accommodated malformed client-tool result: ${error.code || "invalid_tool_result"}; ${error.message}]`;
      return {
        toolCallId: raw && raw.toolCallId,
        content: content ? `${content}\n\n${marker}` : marker,
        isError: true,
      };
    }
  });
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
  const bridgeMarkers = ["\n\n[BRIDGE] STILL RUNNING", "\n\n[BRIDGE] Your Workflow call FAILED"];
  const bridgeMarker = Math.max(...bridgeMarkers.map((marker) => content.lastIndexOf(marker)));
  const candidateSuffix = bridgeMarker >= 0 ? content.slice(bridgeMarker) : "";
  const suffix = Buffer.byteLength(candidateSuffix) <= 16 << 10 ? candidateSuffix : "";
  const head = suffix ? content.slice(0, bridgeMarker) : content;
  const budget = Math.max(0, cap - Buffer.byteLength(suffix));
  let kept = head.slice(0, budget);
  while (Buffer.byteLength(kept) > budget && kept.length) kept = kept.slice(0, -1);
  return kept + `\n[tool result truncated by proxy: kept ${Buffer.byteLength(kept)}/${total} bytes]` + suffix;
}

function sseFrameSizeError(payload, cap = MAX_SSE_FRAME_BYTES) {
  const bytes = Buffer.byteLength(String(payload ?? ""), "utf8");
  return bytes > cap ? new Error(`bridge SSE frame is ${bytes} bytes, exceeding the shared ${cap}-byte limit`) : null;
}

function blocksWithAuthoritativeContent(blocks, content) {
  const source = Array.isArray(blocks) ? blocks : [];
  const replacement = content === ""
    ? null
    : typeof content === "string"
      ? { type: "text", text: content }
      : { type: "json", value: content };
  const out = [];
  let replaced = false;
  for (const block of source) {
    if (block && block.type === "image") {
      out.push(block);
      continue;
    }
    if (!replaced) {
      if (replacement) out.push(replacement);
      replaced = true;
    }
  }
  if (!replaced && replacement) out.unshift(replacement);
  return out;
}
// normalizeClientToolResult gives every tool adapter one result contract (checklist section 3)
// - Preserves failure only when isError === true (never infers from text)
// - Keeps strings, objects, arrays, scalars, null deterministic
// - Separates valid inline base64 images and URL images without dropping either
// - Keeps valid structured content as object
// - Applies Workflow/background augmentations
function normalizeClientToolResult(
  content,
  isError,
  images,
  structuredContent,
  toolName,
  contentBlocks = undefined,
  structuredContentPresent = structuredContent !== undefined,
) {
  const normalized = normalizeToolResultEnvelope(
    content,
    isError,
    images,
    structuredContent,
    contentBlocks,
    structuredContentPresent,
  );
  let normalizedContent = normalized.content;
  // Keep deterministic: don't coerce null to empty, keep as is except undefined -> ""
  // Apply augmentations here (single place)
  if (toolName) {
    if (toolName === "Workflow") {
      normalizedContent = augmentWorkflowResultOnFailure(normalizedContent, normalized.isError);
    }
    normalizedContent = augmentBackgroundLaunchResult(normalizedContent, toolName);
  }
  // Apply live cap to string content (bridge backstop)
  normalizedContent = truncateLiveToolResult(normalizedContent);
  const contentChanged = !Object.is(normalizedContent, normalized.content);
  const normalizedBlocks = contentChanged
    ? blocksWithAuthoritativeContent(normalized.contentBlocks, normalizedContent)
    : normalized.contentBlocks;
  return { ...normalized, content: normalizedContent, contentBlocks: normalizedBlocks };
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
//   prompt (default) -> appended once to the user turn's text
//   rule             -> a system-level always-apply CursorRule via requestContext.rules
//   both             -> both channels (explicit legacy opt-in; duplicates the text)
//   off|0|false|none -> neither
const TOOL_MANIFEST_MODE = (() => {
  const v = String(process.env.CURSOR_COMPOSER_TOOL_MANIFEST ?? "").trim().toLowerCase();
  if (v === "0" || v === "false" || v === "off" || v === "none") return "off";
  if (v === "prompt" || v === "text") return "prompt";
  if (v === "rule" || v === "rules") return "rule";
  if (v === "both") return "both";
  return "prompt";
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
// Invalid calls are rejected inside the bridge before a client ToolRound is
// created. Bound those internal retries separately so a model cannot spin
// forever without advancing the ordinary tool-round counter. This is a count,
// not an upstream deadline.
const COMPOSER_MAX_INVALID_TOOL_CALLS = envInt("CURSOR_COMPOSER_MAX_INVALID_TOOL_CALLS", 32, { min: 1 });
const COMPOSER_MAX_IDENTICAL_INVALID_TOOL_CALLS = envInt("CURSOR_COMPOSER_MAX_IDENTICAL_INVALID_TOOL_CALLS", 8, { min: 2 });
// Observability only: after this much silence from SDK callbacks, emit a debug
// line on the existing SSE keepalive cadence. It never cancels or times out the
// established Cursor stream.
const COMPOSER_STALL_LOG_MS = envInt("CURSOR_COMPOSER_STALL_LOG_MS", 60_000, { min: 1_000 });
function toolManifest(advertised) {
  const adv = Array.isArray(advertised) ? advertised : [];
  if (!adv.length) return "";
  const lines = [];
  let bytes = 0;
  let truncated = false;
  for (const t of adv) {
    const name = (t && (t.toolName || t.name)) || "";
    if (!name) continue;
    // Names only by default; the structural descriptor carries the complete
    // schema. Optional descriptions remain bounded presentation metadata.
    let desc = "";
    if (TOOL_MANIFEST_DESC_MAX > 0 && t && t.description) {
      desc = String(t.description).replace(/\s+/g, " ").trim();
      if (desc.length > TOOL_MANIFEST_DESC_MAX) desc = desc.slice(0, TOOL_MANIFEST_DESC_MAX - 1) + "…";
    }
    const line = desc ? `- ${name}: ${desc}` : `- ${name}`;
    const lineBytes = Buffer.byteLength(line) + 1;
    if (bytes + lineBytes > TOOL_MANIFEST_MAX_BYTES) { truncated = true; break; }
    lines.push(line);
    bytes += lineBytes;
  }
  if (!lines.length) return "";
  if (truncated) lines.push("- …more client tools are available");
  return "Available client tools this turn (reference inventory; it does not replace or extend the user's request):\n"
    + lines.join("\n")
    + "\nThese are direct client capabilities executed by the calling harness on the user's machine. "
    + "Call the matching tool by its exact advertised name and declared schema; do not invent wrapper or transport tool names. "
    + "Invalid calls are not executed. Wait for each returned result, and treat an error result as a real failure rather than success. "
    + "Consume returned content directly; do not create scratch files merely to relay tool output unless the user explicitly requested a file. "
    + "When a task requires delegation and a delegation capability is advertised, call it rather than only narrating delegation. "
    + "Always return to the current user instruction after consulting this inventory.";
}

// toolManifestRule wraps the manifest text in a valid always-apply agent.v1.CursorRule for delivery via
// requestContext.rules (the system-level, per-session channel). Proto shape is exact: { fullPath, content,
// type:{global:{}} (the "always apply" oneof), source, environments[] } — anything else fails the SDK's strict
// fromJson seam. Returns null when there is nothing to advertise.
function toolManifestRule(advertised, cwd) {
  const content = toolManifest(advertised);
  if (!content || !cwd) return null;
  return {
    fullPath: cwd + "/.cursor/rules/cliproxy-tools.mdc",
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
  const adv = sdkAdvertisedTools(st.session);
  // Proves the SDK's tool-advertising path (the non-prewarmed else branch) actually runs per model turn and
  // how many tools it hands the model. If this fires with a full count yet the model still calls only native
  // read/shell, the gap is the MODEL not engaging mcpTools — not a missing advertisement.
  dbg("__CC_GET_ADVERTISE__ called by SDK", "session=" + st.session.id, "returning=" + adv.length + " tools");
  return adv;
};

// Descriptor aliases are private bridge routing metadata. They must be
// available to reconcile native SDK names, but they are not part of Cursor's
// MCP tool descriptor and must never be exposed to the SDK/model.
function sdkAdvertisedTools(session) {
  const advertised = session && session.advertiseForGating
    ? session.advertiseForGating()
    : (session && session.advertise) || [];
  return advertised.map(({ aliases: _aliases, ...tool }) => tool);
}

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

// Headless request context (never goes to CC). Production sessions always carry the validated client path;
// startup self-tests use EMPTY_CWD, never a fake user workspace.
function requestContextEnvelope(ce, ws, cwd, rules) {
  const env = {
    osVersion: ce.osVersion || "linux",
    workspacePaths: ws,
    shell: ce.shell || "bash",
    sandboxEnabled: false,
    timeZone: ce.timeZone || "UTC",
    sandboxSupported: false,
    sandboxNetworkExplicitAllowlist: [],
    computerUseSupported: false,
    isWorkingDirHomeDir: false,
  };
  if (cwd) {
    Object.assign(env, {
      terminalsFolder: cwd + "/.notes/terminals",
      agentSharedNotesFolder: cwd + "/.notes/shared",
      agentConversationNotesFolder: cwd + "/.notes/conv",
      projectFolder: cwd,
      agentTranscriptsFolder: cwd + "/.notes/transcripts",
      processWorkingDirectory: cwd,
    });
  }
  return { __ccJson: { success: { requestContext: {
    rules,
    env,
    repositoryInfo: [], tools: [], conversationNotesListing: "(none)", sharedNotesListing: "(none)",
    gitRepos: [], projectLayouts: [], mcpInstructions: [], fileContents: {}, customSubagents: [],
    commitAttributionMessage: "enabled", prAttributionMessage: "enabled", agentSkills: [],
    precomputedHumanChanges: [], supportsMcpAuth: true, gitRepoInfoComplete: true,
    // Direct advertised tools remain in requestContext.tools. Disable only
    // Cursor's generic GetMcpTools/CallMcpTool discovery wrappers so every
    // harness sees and calls its own tool names directly.
    mcpMetaToolOptions: { enabled: false, mcpDescriptors: [] }, nonFileRules: [] } } } };
}
// headlessRequestContext answers the SDK runtime's requestContextArgs CLIENT request, per session. When the tool
// manifest mode includes "rule", it injects an always-apply CursorRule carrying THIS session's tool manifest, so
// multi-client toolsets never collide (each session's rule reflects its own advertised set). clientEnv comes off
// the session; a null session (e.g. the startup self-test) yields the no-rules envelope.
function headlessRequestContext(session) {
  const ce = (session && session.clientEnv) || {};
  const neutral = ce.workspaceUnknown === true;
  const ws = neutral ? [] : (Array.isArray(ce.workspacePaths) ? ce.workspacePaths : []);
  const cwd = neutral ? "" : (ce.processWorkingDirectory || ws[0] || "");
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

// The advisory workspace cwd reported in shell result metadata. When unknown,
// use the schema's neutral empty string; never expose the SDK isolation cwd.
function composerWorkspaceCwd(clientEnv) {
  const ce = clientEnv || {};
  if (typeof ce.processWorkingDirectory === "string" && ce.processWorkingDirectory) return ce.processWorkingDirectory;
  if (Array.isArray(ce.workspacePaths) && ce.workspacePaths.length && typeof ce.workspacePaths[0] === "string" && ce.workspacePaths[0]) return ce.workspacePaths[0];
  if (ce.workspaceUnknown === true) return "";
  return "";
}

function validClientEnv(clientEnv) {
  if (!clientEnv || typeof clientEnv !== "object") return false;
  const cwd = clientEnv.processWorkingDirectory;
  const ws = clientEnv.workspacePaths;
  if (clientEnv.workspaceUnknown === true) return cwd == null && (ws == null || (Array.isArray(ws) && ws.length === 0));
  return typeof cwd === "string" && cwd.length > 0 &&
    Array.isArray(ws) && ws.length > 0 && ws.every((item) => typeof item === "string" && item.length > 0);
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
  // A harness normally returns a human status string (for example, a path/hash plus byte count) rather
  // than the post-write bytes. A plain string therefore cannot prove actual
  // content and must never be reported back to Cursor as fileContentAfterWrite.
  // Use the requested bytes; only the structured envelope above is an explicit
  // actual-content channel.
  const actual = requested;
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
  shellArgs:       { ccTool: "shell", stream: false, buildResult: (c, s, isError, ctx) => { const r = parseShellContent(c); const code = isError && r.exitCode === 0 ? 1 : r.exitCode; return { success: { command: s && s.command, workingDirectory: (ctx && ctx.cwd) || "", exitCode: code, stdout: r.stdout, stderr: r.stderr } }; } },
  shellStreamArgs: { ccTool: "shell", stream: true,  buildChunks: (c, isError, ctx) => { const r = parseShellContent(c); const code = isError && r.exitCode === 0 ? 1 : r.exitCode; const aborted = isError ? true : r.aborted; const out = [{ stdout: { data: r.stdout } }]; if (r.stderr) out.push({ stderr: { data: r.stderr } }); out.push({ exit: { code, cwd: (ctx && ctx.cwd) || "", aborted, localExecutionTimeMs: 1 } }); return out; } },
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
function mcpStructuredProjection(structuredContent, structuredContentPresent) {
  if (!structuredContentPresent) return { marker: null, structuredContent: undefined };
  if (structuredContent && typeof structuredContent === "object" && !Array.isArray(structuredContent)) {
    return { marker: null, structuredContent };
  }
  return { marker: `[Structured content: ${JSON.stringify(structuredContent)}]`, structuredContent: undefined };
}

function mcpDispatchResult(content, isError, images, structuredContent, contentBlocks, structuredContentPresent = structuredContent !== undefined) {
  const text = typeof content === "string" ? content : JSON.stringify(content ?? "");
  const parts = [];
  if (Array.isArray(contentBlocks)) {
    for (const block of contentBlocks) {
      if (block && block.type === "text" && typeof block.text === "string") {
        parts.push({ text: { text: block.text } });
      } else if (block && block.type === "json") {
        parts.push({ text: { text: JSON.stringify(block.value) } });
      } else if (block && block.type === "image" && block.source && block.source.kind === "base64") {
        parts.push({ image: { data: block.source.data, mimeType: block.source.mimeType } });
      } else if (block && block.type === "image" && block.source && block.source.kind === "url") {
        parts.push({ text: { text: `[Image URL: ${block.source.url}]` } });
      }
    }
  } else if (Array.isArray(images)) {
    for (const im of images) {
      if (im && typeof im.data === "string" && im.data && typeof im.mimeType === "string" && im.mimeType) {
        parts.push({ image: { data: im.data, mimeType: im.mimeType } });
      }
    }
  }
  if (!Array.isArray(contentBlocks) && (text || parts.length === 0)) parts.push({ text: { text } });
  if (parts.length === 0) parts.push({ text: { text: "" } });
  const result = { success: { isError: isError === true, content: parts } };
  const structured = mcpStructuredProjection(structuredContent, structuredContentPresent);
  if (structured.structuredContent !== undefined) result.success.structuredContent = structured.structuredContent;
  if (structured.marker !== null) result.success.content.push({ text: { text: structured.marker } });
  return result;
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

// Cursor sometimes tries to spill a large MCP result through its built-in
// filesystem tools using agent-tools/<uuid>.txt, then reads that file back.
// In a remote client-tools deployment that path is an internal Cursor
// artifact, not a user-requested workspace operation. Forwarding it makes the
// harness create implementation debris on the user's machine and can start a
// write/read/shell loop. Block only that unmistakable UUID artifact shape;
// ordinary user Read/Write/Bash calls continue through ToolRound unchanged.
const SYNTHETIC_AGENT_ARTIFACT_FILE_RE = /(?:^|[\\/\s'"=])agent-tools[\\/][0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\.txt(?:$|[\s'"?#])/i;
const SYNTHETIC_AGENT_ARTIFACT_PATH_RE = /[^\s'"=]*agent-tools[\\/][0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\.txt/ig;
const SYNTHETIC_AGENT_ARTIFACT_FAMILIES = new Set(["read", "write", "shell"]);
const SYNTHETIC_AGENT_ARTIFACT_DISABLED_MSG =
  "Cursor's internal agent-tools artifact handoff is disabled for remote client tools. " +
  "The MCP/tool result is already available in this conversation; use it directly and do not write, read, list, or shell-inspect an agent-tools file.";
const SYNTHETIC_AGENT_ARTIFACT_PATH_KEYS = ["path", "file_path", "filePath"];
const SYNTHETIC_AGENT_ARTIFACT_CONTENT_KEYS = ["content", "fileText", "file_text", "text"];
const SYNTHETIC_AGENT_ARTIFACT_COMMAND_KEYS = ["command", "cmd"];

function syntheticAgentArtifactRequest(name, input) {
  if (!SYNTHETIC_AGENT_ARTIFACT_FAMILIES.has(clientToolFamily(name))) return false;
  const value = input && typeof input === "object" ? input : {};
  for (const key of ["path", "file_path", "filePath", "command", "cmd"]) {
    if (typeof value[key] === "string" && SYNTHETIC_AGENT_ARTIFACT_FILE_RE.test(value[key])) return true;
  }
  return false;
}

function syntheticAgentArtifactFailure(name, input) {
  if (!syntheticAgentArtifactRequest(name, input)) return null;
  return {
    content: SYNTHETIC_AGENT_ARTIFACT_DISABLED_MSG,
    images: [],
    isError: true,
    structuredContent: { code: "synthetic_agent_artifact_disabled" },
  };
}

function artifactInputPaths(input) {
  const value = input && typeof input === "object" ? input : {};
  const paths = SYNTHETIC_AGENT_ARTIFACT_PATH_KEYS
    .map((key) => value[key])
    .filter((candidate) => typeof candidate === "string" && candidate.length > 0);
  for (const key of SYNTHETIC_AGENT_ARTIFACT_COMMAND_KEYS) {
    if (typeof value[key] !== "string") continue;
    for (const candidate of value[key].match(SYNTHETIC_AGENT_ARTIFACT_PATH_RE) || []) paths.push(candidate);
  }
  return [...new Set(paths)];
}

function artifactInputText(input, keys) {
  const value = input && typeof input === "object" ? input : {};
  for (const key of keys) if (typeof value[key] === "string") return value[key];
  return "";
}

function artifactContentHashes(content) {
  if (typeof content !== "string" || Buffer.byteLength(content) < SYNTHETIC_ARTIFACT_RESULT_MIN_BYTES) return [];
  const variants = new Set([content, content.replace(/\r\n/g, "\n")]);
  for (const value of [...variants]) variants.add(value.replace(/\n+$/g, ""));
  return [...variants].map((value) => createHash("sha256").update(value).digest("hex"));
}

function syntheticArtifactFailure(decision) {
  const repeat = decision.attempt > 1
    ? ` This internal handoff has already been rejected ${decision.attempt} times; stop retrying it.`
    : "";
  return {
    content: `${SYNTHETIC_AGENT_ARTIFACT_DISABLED_MSG}${repeat}`,
    images: [],
    isError: true,
    structuredContent: {
      code: "synthetic_agent_artifact_disabled",
      reason: decision.reason,
      attempt: decision.attempt,
    },
  };
}

// blockedNativeResult builds a typed FAILURE result for a native tool the model tried to use while tool_choice
// disallowed it (H08). It uses the SAME failure channels as a real failed tool — the `error` variant for
// read/write/delete and a non-zero/aborted exit for shell — so the model SEES the block and chooses another
// path, never a fabricated success. Returns the unary { __ccJson } shape; the streaming case yields chunks.
const NATIVE_TOOL_DISABLED_MSG = "tool disabled for this turn by tool_choice policy";
function blockedNativeResult(cas, s, cwd = "") {
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
    if (store && store.session) store.session.recordInternalToolRejection("typed-unavailable", cas, { settleAnnounced: true });
    return Promise.resolve(typedUnavailableResult(cas));
  }
  return null;
}

function blockedNativeExecIfNeeded(store, cas, s, stream) {
  if (!store || !nativeToolBlockedByChoice(store.session.toolChoice)) return null;
  const cwd = composerWorkspaceCwd(store.session.clientEnv);
  dbg("__CC_EXEC_" + (stream ? "S" : "U") + " native tool blocked by tool_choice", "session=" + store.session.id, "cas=" + cas, "toolChoice=" + store.session.toolChoice);
  store.session.recordInternalToolRejection("tool-choice", cas, { settleAnnounced: true, input: s });
  if (stream) {
    return (async function* () { yield { __ccJson: { stdout: { data: "" } } }; yield { __ccJson: { exit: { code: 1, cwd, aborted: true, localExecutionTimeMs: 1 } } }; })();
  }
  return Promise.resolve(blockedNativeResult(cas, s, cwd));
}

function blockedSyntheticNativeExecIfNeeded(store, cas, s, stream) {
  const spec = CC_CASES[cas];
  if (!store || !spec) return null;
  const input = ccArgsFor(cas, s);
  const failure = store.session.syntheticArtifactFailure(spec.ccTool, input);
  if (!failure) return null;
  const cwd = composerWorkspaceCwd(store.session.clientEnv);
  dbg("__CC_EXEC_" + (stream ? "S" : "U") + " internal artifact blocked at SDK dispatch", "session=" + store.session.id, "cas=" + cas);
  store.session.recordInternalToolRejection("synthetic-artifact", cas, { settleAnnounced: true, input });
  if (stream) {
    return (async function* () {
      for (const chunk of spec.buildChunks(failure.content, true, { cwd })) yield { __ccJson: chunk };
    })();
  }
  return Promise.resolve({ __ccJson: spec.buildResult(failure.content, s, true, { cwd }) });
}

globalThis.__CC_EXEC_U = function (n, e, s, t) {
  const cas = caseOf(t);
  const store = als.getStore();
  const preflight = unaryExecPreflight(cas, store);
  if (preflight) return preflight;
  const synthetic = blockedSyntheticNativeExecIfNeeded(store, cas, s, false);
  if (synthetic) return synthetic;
  if (cas === "mcpArgs") {
    if (!store) return Promise.reject(new Error("[bridge] mcpArgs outside a session"));
    return store.session.dispatchMcp(s);
  }
  const spec = CC_CASES[cas];
  if (!spec || spec.stream || !spec.buildResult || !store) {
    return Promise.reject(new Error(`[bridge] tool '${cas}' is not supported by the client-tools bridge`));
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
      return (async function* () { throw new Error(`[bridge] streaming tool '${cas}' is not supported by the client-tools bridge`); })();
  }
  const synthetic = blockedSyntheticNativeExecIfNeeded(store, cas, s, true);
  if (synthetic) return synthetic;
  const blocked = blockedNativeExecIfNeeded(store, cas, s, true);
  if (blocked) return blocked;
  return store.session.dispatchStream(cas, spec, s);
};

// ---- session: holds the live SDK run + bridges tool calls across /agent/turn calls ----
const sessions = new Map();
const liveToolRounds = new Map(); // signed route -> ToolRound with live SDK callbacks in this process
let roundInfrastructure = null;
let bridgeReady = false;
function getRoundInfrastructure() {
  if (!roundInfrastructure) roundInfrastructure = createRoundInfrastructure(STATE_ROOT);
  return roundInfrastructure;
}
function loadToolRoundById(toolCallId) {
  const { codec, journal } = getRoundInfrastructure();
  const parsed = codec.parse(toolCallId);
  const live = liveToolRounds.get(parsed.route);
  if (live) return { parsed, round: live, live: true };
  const round = ToolRound.load(journal, codec, parsed.route);
  return { parsed, round, live: false };
}
// Bound the sessions map (no unbounded growth): evict least-recently-active, IDLE sessions over the cap. A
// session is idle-evictable ONLY when it has no open response, no live/paused run, AND no queued waiters — so
// we never evict a session that is streaming or paused awaiting tool_results (its SDK run is live between
// HTTP turns) or has work queued behind it.
function enforceSessionCap() {
  if (sessions.size <= MAX_SESSIONS) return;
  const evictable = [...sessions.values()].filter((s) => !s.activeRes && !s.run && !s.hasQueuedWaiters()).sort((a, b) => a.lastActivity - b.lastActivity);
  for (const s of evictable) {
    if (sessions.size <= MAX_SESSIONS) break;
    sessions.delete(s.id);
    cancelSessionDetached(s, { terminalReason: TerminalReason.SESSION_EVICTED, detail: "session capacity eviction" });
  }
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

// A recovery or rotation can deliberately move one external conversation onto a replacement durable Cursor
// agent. Persist that indirection under STATE_ROOT so a bridge restart does not silently fall back to the
// original agent and lose the recovered context. The credential itself is never written; its full one-way
// fingerprint scopes the alias. Unlike the history fingerprint (an optimization), this mapping is continuity
// state and therefore fails closed on corrupt/unreadable records.
const AGENT_ALIAS_DIR = path.join(STATE_ROOT, ".cct-agent-alias");
const COMPLETED_TURN_DIR = path.join(STATE_ROOT, ".cct-completed-turns");
const completedTurnReceipts = new Map();
const TURN_IDENTITY_POLICY = Object.freeze({
  INVOCATION_V1: "invocation-v1",
  LEGACY_CLIENT_MESSAGE_V1: "legacy-client-message-v1",
  NONE: "none",
});
// Dual-read alias for legacy v4/v5 status values. New writes use ACCEPTANCE_PHASE.
const FRESH_ATTEMPT_STATE = LEGACY_FRESH_ATTEMPT_STATE;
const INVOCATION_ID_RE = /^[A-Za-z0-9][A-Za-z0-9._:-]{7,255}$/;
const SYSTEM_BLOCK_ID_RE = /^[A-Za-z0-9][A-Za-z0-9._:-]{7,127}$/;
const MAX_SYSTEM_BLOCKS = 4096;
function validSystemBlockIds(value) {
  return Array.isArray(value)
    && value.length <= MAX_SYSTEM_BLOCKS
    && value.every((id) => typeof id === "string" && SYSTEM_BLOCK_ID_RE.test(id))
    && new Set(value).size === value.length;
}
function normalizeSystemBlocks(input) {
  const value = input && typeof input === "object" ? input : {};
  if (!Object.prototype.hasOwnProperty.call(value, "systemBlocks")) return null;
  if (!Array.isArray(value.systemBlocks) || value.systemBlocks.length > MAX_SYSTEM_BLOCKS) {
    throw new ToolRoundError("invalid_system_blocks", "systemBlocks must be a bounded array", 422);
  }
  const blocks = value.systemBlocks.map((block) => {
    if (!block || typeof block !== "object" || Array.isArray(block)
        || typeof block.id !== "string" || !SYSTEM_BLOCK_ID_RE.test(block.id)
        || typeof block.text !== "string" || !block.text) {
      throw new ToolRoundError(
        "invalid_system_blocks",
        "every system block requires an opaque 8-128 character id and non-empty text",
        422,
      );
    }
    return { id: block.id, text: block.text };
  });
  const ids = blocks.map((block) => block.id);
  if (!validSystemBlockIds(ids)) {
    throw new ToolRoundError("invalid_system_blocks", "system block ids must be unique", 422);
  }
  const aggregate = blocks.map((block) => block.text).join("\n\n");
  if (aggregate !== String(value.system || "")) {
    throw new ToolRoundError(
      "invalid_system_blocks",
      "systemBlocks must exactly describe the aggregate system text",
      422,
    );
  }
  return blocks;
}
function systemBlockIds(input) {
  const blocks = normalizeSystemBlocks(input);
  return blocks === null ? null : blocks.map((block) => block.id);
}
function sameStringArray(left, right) {
  return Array.isArray(left) && Array.isArray(right)
    && left.length === right.length
    && left.every((value, index) => value === right[index]);
}
function isStringArrayPrefix(prefix, whole) {
  return Array.isArray(prefix) && Array.isArray(whole)
    && prefix.length <= whole.length
    && prefix.every((value, index) => value === whole[index]);
}
function agentAliasPathFor(cursorKey, sessionId) {
  const scope = createHash("sha256")
    .update(keyFingerprint(cursorKey))
    .update("\0")
    .update(String(sessionId || ""))
    .digest("hex");
  return path.join(AGENT_ALIAS_DIR, `${scope}.json`);
}
function validateDurableAgentAlias(record, cursorKey, sessionId) {
  const expectedSessionId = String(sessionId || "");
  const epochValid = (value) => value === undefined
    || (Number.isSafeInteger(value) && value >= 0 && value <= 1024);
  if (!record || typeof record !== "object" || Array.isArray(record)
      || record.version !== 1
      || record.keyFingerprint !== keyFingerprint(cursorKey)
      || record.sessionId !== expectedSessionId
      || typeof record.agentId !== "string"
      || record.agentId.length === 0
      || record.agentId.length > 512
      || !record.agentId.startsWith(`${expectedSessionId}_`)
      || !epochValid(record.recoveryEpoch)
      || !epochValid(record.modelEpoch)
      || !epochValid(record.keyEpoch)
      || !epochValid(record.contextEpoch)
      || (record.systemBlockIds !== undefined && !validSystemBlockIds(record.systemBlockIds))) {
    throw new Error("durable Cursor agent alias is malformed or belongs to another session/credential");
  }
  return record;
}
function readDurableAgentAlias(cursorKey, sessionId) {
  const file = agentAliasPathFor(cursorKey, sessionId);
  let raw;
  try {
    raw = readFileSync(file, "utf8");
  } catch (error) {
    if (error && error.code === "ENOENT") return null;
    throw new Error(`cannot read durable Cursor agent alias: ${(error && error.message) || String(error)}`);
  }
  try {
    return validateDurableAgentAlias(JSON.parse(raw), cursorKey, sessionId);
  } catch (error) {
    throw new Error(`cannot validate durable Cursor agent alias: ${(error && error.message) || String(error)}`);
  }
}
function writeDurableAgentAlias(cursorKey, sessionId, agentId, epochs = {}) {
  const record = {
    version: 1,
    keyFingerprint: keyFingerprint(cursorKey),
    sessionId: String(sessionId || ""),
    agentId: String(agentId || ""),
    recoveryEpoch: Number.isSafeInteger(epochs.recoveryEpoch) ? epochs.recoveryEpoch : 0,
    modelEpoch: Number.isSafeInteger(epochs.modelEpoch) ? epochs.modelEpoch : 0,
    keyEpoch: Number.isSafeInteger(epochs.keyEpoch) ? epochs.keyEpoch : 0,
    contextEpoch: Number.isSafeInteger(epochs.contextEpoch) ? epochs.contextEpoch : 0,
    systemBlockIds: validSystemBlockIds(epochs.systemBlockIds) ? [...epochs.systemBlockIds] : [],
  };
  validateDurableAgentAlias(record, cursorKey, sessionId);
  mkdirSync(AGENT_ALIAS_DIR, { recursive: true, mode: 0o700 });
  const file = agentAliasPathFor(cursorKey, sessionId);
  const temp = `${file}.${process.pid}.${randomUUID()}.tmp`;
  const bytes = Buffer.from(JSON.stringify(record) + "\n", "utf8");
  let fd = null;
  try {
    fd = openSync(temp, "wx", 0o600);
    let offset = 0;
    while (offset < bytes.length) offset += writeSync(fd, bytes, offset, bytes.length - offset);
    fsyncSync(fd);
    closeSync(fd);
    fd = null;
    renameSync(temp, file);
    const dirFD = openSync(AGENT_ALIAS_DIR, "r");
    try { fsyncSync(dirFD); } finally { closeSync(dirFD); }
    const persisted = readDurableAgentAlias(cursorKey, sessionId);
    if (persisted.agentId !== record.agentId
        || persisted.recoveryEpoch !== record.recoveryEpoch
        || persisted.modelEpoch !== record.modelEpoch
        || persisted.keyEpoch !== record.keyEpoch
        || persisted.contextEpoch !== record.contextEpoch
        || !sameStringArray(persisted.systemBlockIds, record.systemBlockIds)) {
      throw new Error("durable Cursor agent alias verification failed");
    }
  } catch (error) {
    if (fd !== null) {
      try { closeSync(fd); } catch {}
    }
    try { rmSync(temp, { force: true }); } catch {}
    throw new Error(`cannot persist durable Cursor agent alias: ${(error && error.message) || String(error)}`);
  }
}

function recoveryTargetAgentId(round) {
  const source = String(round?.agentId || round?.sessionId || "");
  const stableSource = source.replace(/_ctrecover_[A-Za-z0-9_-]+$/, "");
  return `${stableSource}_ctrecover_${String(round?.route || "").slice(0, 10)}`;
}

// Invocation identity and semantic request equivalence are deliberately
// separate contracts. invocationId is minted by the harness once per
// independently initiated provider call and remains stable only across that
// call's transport retries. Old clients did not send it, so the explicitly
// versioned compatibility policy falls back to clientMessageId. A supplied but
// malformed invocationId is never silently downgraded: doing so would merge an
// independently initiated turn with a content-derived legacy identity.
function turnInvocationIdentity(input) {
  const value = input && typeof input === "object" ? input : {};
  if (Object.prototype.hasOwnProperty.call(value, "invocationId")) {
    if (typeof value.invocationId !== "string" || !INVOCATION_ID_RE.test(value.invocationId)) {
      throw new ToolRoundError(
        "invalid_invocation_id",
        "invocationId must be an opaque 8-256 character token containing only letters, digits, dot, underscore, colon, or hyphen",
        422,
      );
    }
    return { id: value.invocationId, policy: TURN_IDENTITY_POLICY.INVOCATION_V1 };
  }
  const legacy = typeof value.clientMessageId === "string" ? value.clientMessageId.trim() : "";
  if (legacy) {
    if (legacy.length > 512 || /[\u0000-\u001f\u007f]/.test(legacy)) {
      throw new ToolRoundError("invalid_client_message_id", "clientMessageId is malformed", 422);
    }
    return { id: legacy, policy: TURN_IDENTITY_POLICY.LEGACY_CLIENT_MESSAGE_V1 };
  }
  return { id: "", policy: TURN_IDENTITY_POLICY.NONE };
}

function versionedFreshReplayIdentity(identity) {
  return identity.policy === TURN_IDENTITY_POLICY.INVOCATION_V1
    || (identity.policy === TURN_IDENTITY_POLICY.LEGACY_CLIENT_MESSAGE_V1
      && /^ccm2_[A-Za-z0-9_-]+$/.test(identity.id));
}

function completedTurnRequestHash(input) {
  const value = input && typeof input === "object" ? input : {};
  const normalizedSystemBlocks = normalizeSystemBlocks(value);
  const results = (Array.isArray(value.results) ? value.results : []).map((result) => ({
    toolCallId: typeof result?.toolCallId === "string" ? result.toolCallId : "",
    result: canonicalResult(result),
  })).sort((a, b) => {
    const byId = a.toolCallId.localeCompare(b.toolCallId);
    return byId || canonicalJSONString(a.result).localeCompare(canonicalJSONString(b.result));
  });
  // Do not include invocationId/clientMessageId: they answer "which logical
  // call is this?", while this digest proves the complete model-visible
  // payload is equivalent. Keeping the two axes separate is what lets an exact
  // retry replay while an independently initiated identical turn stays unique.
  const semantic = {
    history: typeof value.history === "string" ? value.history : "",
    historyFingerprint: typeof value.historyFingerprint === "string" ? value.historyFingerprint : "",
    images: Array.isArray(value.images) ? value.images : [],
    interruptRequested: value.interruptRequested === true,
    legacyUnsignedReplay: value.legacyUnsignedReplay === true,
    recoveryContext: typeof value.recoveryContext === "string" ? value.recoveryContext : "",
    results,
    system: typeof value.system === "string" ? value.system : "",
    systemBlocks: normalizedSystemBlocks || [],
    text: typeof value.text === "string" ? value.text : "",
    type: typeof value.type === "string" ? value.type : "",
    userText: typeof value.userText === "string" ? value.userText : "",
    version: 2,
  };
  return createHash("sha256").update(canonicalJSONString(semantic)).digest("hex");
}

function invocationBindingPath(cursorKey, sessionId, invocationId) {
  const scope = createHash("sha256")
    .update(keyFingerprint(cursorKey))
    .update("\0")
    .update(String(sessionId || ""))
    .update("\0")
    .update(String(invocationId || ""))
    .digest("hex");
  return path.join(COMPLETED_TURN_DIR, `${scope}.binding.json`);
}

// Bind an explicit invocation identity to exactly one semantic payload before
// any session mutation. Atomic link publication makes concurrent replicas
// converge on the first binding; reuse with changed content is a conflict,
// never a second SDK send.
function bindInvocationRequestHash(cursorKey, sessionId, identity, requestHash) {
  if (!identity || identity.policy !== TURN_IDENTITY_POLICY.INVOCATION_V1) return null;
  if (!/^[a-f0-9]{64}$/.test(requestHash)) throw new Error("invocation request hash is invalid");
  const file = invocationBindingPath(cursorKey, sessionId, identity.id);
  const validate = (record) => !!record && record.version === 1
    && record.status === "identity_bound"
    && record.keyFingerprint === keyFingerprint(cursorKey)
    && record.sessionId === String(sessionId || "")
    && record.invocationId === identity.id
    && /^[a-f0-9]{64}$/.test(record.requestHash);
  const readWinner = () => {
    let record;
    try { record = JSON.parse(readFileSync(file, "utf8")); }
    catch (error) {
      if (error && error.code === "ENOENT") return null;
      throw new Error(`cannot read invocation binding: ${(error && error.message) || String(error)}`);
    }
    if (!validate(record)) throw new Error("durable invocation binding is malformed");
    if (record.requestHash !== requestHash) {
      throw new ToolRoundError(
        "invocation_payload_conflict",
        "the invocation identity is already bound to a different request payload",
        409,
      );
    }
    return record;
  };
  const existing = readWinner();
  if (existing) return existing;
  const record = {
    version: 1,
    status: "identity_bound",
    keyFingerprint: keyFingerprint(cursorKey),
    sessionId: String(sessionId || ""),
    invocationId: identity.id,
    requestHash,
    boundAt: nowMs(),
  };
  mkdirSync(COMPLETED_TURN_DIR, { recursive: true, mode: 0o700 });
  const temp = `${file}.${process.pid}.${randomUUID()}.tmp`;
  let fd = null;
  try {
    const bytes = Buffer.from(JSON.stringify(record) + "\n", "utf8");
    fd = openSync(temp, "wx", 0o600);
    let offset = 0;
    while (offset < bytes.length) offset += writeSync(fd, bytes, offset, bytes.length - offset);
    fsyncSync(fd);
    closeSync(fd);
    fd = null;
    linkSync(temp, file);
    rmSync(temp, { force: true });
    const dirFD = openSync(COMPLETED_TURN_DIR, "r");
    try { fsyncSync(dirFD); } finally { closeSync(dirFD); }
    return record;
  } catch (error) {
    if (fd !== null) { try { closeSync(fd); } catch {} }
    try { rmSync(temp, { force: true }); } catch {}
    const winner = readWinner();
    if (winner) return winner;
    throw new Error(`cannot persist invocation binding: ${(error && error.message) || String(error)}`);
  }
}

function canonicalReplayEvent(event) {
  if (!event || typeof event !== "object" || Array.isArray(event)) {
    throw new Error("completed replay event must be an object");
  }
  const value = JSON.parse(canonicalJSONString(event));
  if (value.type === "text" || value.type === "reasoning") {
    if (typeof value.delta !== "string") throw new Error(`${value.type} replay event requires a string delta`);
    return { type: value.type, delta: value.delta };
  }
  if (value.type === "tool_call") {
    if (typeof value.id !== "string" || !value.id || typeof value.name !== "string" || !value.name) {
      throw new Error("tool_call replay event requires id and name");
    }
    return {
      type: "tool_call",
      id: value.id,
      name: value.name,
      input: Object.prototype.hasOwnProperty.call(value, "input") ? value.input : {},
    };
  }
  if (value.type === "turn_end") {
    if (value.stop_reason !== "end_turn") {
      throw new Error("a completed replay log must end with turn_end/end_turn");
    }
    const { _clientLeaseTerminal, ...terminal } = value;
    return terminal;
  }
  throw new Error(`unsupported completed replay event type ${String(value.type || "")}`);
}

function canonicalReplayEvents(events) {
  if (!Array.isArray(events) || events.length === 0) {
    throw new Error("completed replay log is empty");
  }
  const canonical = events.map(canonicalReplayEvent);
  const terminal = canonical.at(-1);
  if (!terminal || terminal.type !== "turn_end" || terminal.stop_reason !== "end_turn") {
    throw new Error("completed replay log has no final terminal");
  }
  if (canonical.slice(0, -1).some((event) => event.type === "turn_end")) {
    throw new Error("completed replay log contains an early terminal");
  }
  if (Buffer.byteLength(canonicalJSONString(canonical), "utf8") > MAX_AGENT_TURN_BYTES) {
    throw new Error("completed replay event log exceeds the durable replay bound");
  }
  return canonical;
}

function completedTurnReceiptPath(cursorKey, sessionId, clientMessageId, requestHash = "") {
  const scope = createHash("sha256")
    .update(keyFingerprint(cursorKey))
    .update("\0")
    .update(String(sessionId || ""))
    .update("\0")
    .update(String(clientMessageId || ""))
    .update(requestHash ? "\0" : "")
    .update(requestHash)
    .digest("hex");
  return path.join(COMPLETED_TURN_DIR, `${scope}.json`);
}

const receiptCAS = new DurableJSONCAS();
const unresolvedReservationFile = path.join(COMPLETED_TURN_DIR, ".unresolved-reservations.json");

function mutateUnresolvedReservations(reducer) {
  for (let attempts = 0; attempts < 64; attempts++) {
    const state = receiptCAS.readRecover(unresolvedReservationFile);
    const current = state.record || { version: 1, entries: {} };
    const next = reducer(current);
    if (!next) return current;
    try {
      const committed = receiptCAS.commit(unresolvedReservationFile, state, next).record;
      receiptCAS.cleanupArtifacts(unresolvedReservationFile, { keepRevision: committed.revision });
      return committed;
    } catch (error) {
      if (error instanceof DurableCASConflict) continue;
      throw error;
    }
  }
  throw new ToolRoundError("durable_state_capacity", "durable reservation ledger is changing too quickly", 503);
}

function reserveUnresolvedReceipt(file, bytes) {
  const key = path.basename(file);
  return mutateUnresolvedReservations((current) => {
    const entries = current.entries && typeof current.entries === "object" ? current.entries : {};
    const existingBytes = Math.max(0, Number(entries[key]?.bytes) || 0);
    if (existingBytes >= bytes) return null;
    const reserved = Object.values(entries).reduce((sum, entry) => sum + Math.max(0, Number(entry?.bytes) || 0), 0);
    const additional = bytes - existingBytes;
    const disk = stateRootDiskStatus();
    if (reserved + additional > UNRESOLVED_RECEIPT_MAX_BYTES
        || disk.freeBytes - reserved - additional < STATE_ROOT_MIN_FREE_BYTES) {
      throw new ToolRoundError(
        "durable_state_capacity",
        "shared durable receipt capacity is occupied; retry this turn after prior uncertain deliveries resolve",
        507,
      );
    }
    return {
      version: 1,
      entries: { ...entries, [key]: { bytes, createdAt: entries[key]?.createdAt || nowMs() } },
    };
  });
}

function resizeUnresolvedReceipt(file, bytes) {
  const key = path.basename(file);
  mutateUnresolvedReservations((current) => {
    const entries = current.entries && typeof current.entries === "object" ? current.entries : {};
    if (!entries[key] || entries[key].bytes === bytes) return null;
    return {
      version: 1,
      entries: { ...entries, [key]: { ...entries[key], bytes } },
    };
  });
}

function releaseUnresolvedReceipt(file) {
  const key = path.basename(file);
  mutateUnresolvedReservations((current) => {
    const entries = current.entries && typeof current.entries === "object" ? current.entries : {};
    if (!entries[key]) return null;
    const next = { ...entries };
    delete next[key];
    return { version: 1, entries: next };
  });
}

function releasePreReservationWithoutReceipt(input) {
  const file = typeof input?.preReservedReceiptFile === "string" ? input.preReservedReceiptFile : "";
  if (!file) return;
  try {
    const record = readTurnReceiptFile(file);
    if (unresolvedFreshDeliveryReceipt(record) || record?.status === "completed") return;
  } catch (error) {
    if (!error || error.code !== "ENOENT") return;
  }
  try { releaseUnresolvedReceipt(file); } catch {}
  delete input.preReservedReceiptFile;
}

function reconcileUnresolvedReservations() {
  const observed = {};
  try {
    for (const name of readdirSync(COMPLETED_TURN_DIR)) {
      if (!/^[a-f0-9]{64}\.json$/.test(name)) continue;
      const file = path.join(COMPLETED_TURN_DIR, name);
      try {
        const record = readTurnReceiptFile(file);
        if (unresolvedFreshDeliveryReceipt(record)) {
          observed[name] = { bytes: statSync(file).size, createdAt: Number(record.startedAt) || nowMs() };
        }
      } catch {}
    }
  } catch (error) {
    if (!error || error.code !== "ENOENT") throw error;
  }
  mutateUnresolvedReservations((current) => {
    const entries = current.entries && typeof current.entries === "object" ? current.entries : {};
    const cutoff = nowMs() - UNRESOLVED_RESERVATION_ORPHAN_MS;
    for (const [name, entry] of Object.entries(entries)) {
      if (!observed[name] && Number(entry?.createdAt) >= cutoff) observed[name] = entry;
    }
    if (canonicalJSONString(entries) === canonicalJSONString(observed)) return null;
    return { version: 1, entries: observed };
  });
}

function readTurnReceiptFile(file) {
  const record = receiptCAS.readRecover(file).record;
  if (record) return record;
  const error = new Error(`durable turn receipt ${file} does not exist`);
  error.code = "ENOENT";
  throw error;
}

function mutateTurnReceipt(file, reducer) {
  for (let attempts = 0; attempts < 64; attempts++) {
    const state = receiptCAS.readRecover(file);
    const decision = reducer(state.record);
    if (!decision || decision.commit === false) return decision ? decision.record : state.record;
    try {
      const committed = receiptCAS.commit(file, state, decision.record).record;
      receiptCAS.cleanupArtifacts(file, { keepRevision: committed.revision });
      return committed;
    } catch (error) {
      if (error instanceof DurableCASConflict) continue;
      throw error;
    }
  }
  throw new ToolRoundError(
    "durable_receipt_update_in_progress",
    "durable turn receipt changed too many times while committing; retry the identical request",
    503,
  );
}

function validCompletedTurnReceipt(record, cursorKey, sessionId, clientMessageId, requestHash = "") {
  const versionOk = !!record && typeof record === "object" && !Array.isArray(record)
    && (record.version === 1 || record.version === 2 || record.version === 3 || record.version === 4
      || record.version === 5 || record.version === TURN_RECEIPT_VERSION);
  const completedPhase = resolveAcceptancePhase(record) === ACCEPTANCE_PHASE.COMPLETED
    || record?.status === "completed";
  const common = versionOk
    && completedPhase
    && record.keyFingerprint === keyFingerprint(cursorKey)
    && record.sessionId === String(sessionId || "")
    && record.clientMessageId === String(clientMessageId || "")
    && (record.version === 1
      // An unscoped v1 receipt cannot prove equivalence to a current request.
      // Never replay it merely because the client id happens to match.
      ? !requestHash || (typeof record.requestHash === "string" && record.requestHash === requestHash)
      : /^[a-f0-9]{64}$/.test(record.requestHash) && (!requestHash || record.requestHash === requestHash))
    && (record.version < 3 || (Number.isSafeInteger(record.generation) && record.generation >= 1))
    && record.usage && typeof record.usage === "object" && !Array.isArray(record.usage);
  if (!common) return false;
  if (record.version < 4) {
    return typeof record.text === "string"
      && Buffer.byteLength(record.text, "utf8") <= MAX_AGENT_TURN_BYTES;
  }
  if (record.identityPolicy !== TURN_IDENTITY_POLICY.INVOCATION_V1
      && record.identityPolicy !== TURN_IDENTITY_POLICY.LEGACY_CLIENT_MESSAGE_V1) return false;
  try {
    canonicalReplayEvents(record.events);
    return true;
  } catch {
    return false;
  }
}

function readCompletedTurnReceipt(cursorKey, sessionId, clientMessageId, requestHash = "") {
  if (!clientMessageId) return null;
  const scopedFile = requestHash
    ? completedTurnReceiptPath(cursorKey, sessionId, clientMessageId, requestHash)
    : "";
  const files = requestHash
    ? [
      scopedFile,
      completedTurnReceiptPath(cursorKey, sessionId, clientMessageId),
    ]
    : [completedTurnReceiptPath(cursorKey, sessionId, clientMessageId)];
  for (const file of files) {
    const cached = completedTurnReceipts.get(file);
    if (cached && validCompletedTurnReceipt(cached, cursorKey, sessionId, clientMessageId, requestHash)) return cached;
    try {
      const record = readTurnReceiptFile(file);
      if (!validCompletedTurnReceipt(record, cursorKey, sessionId, clientMessageId, requestHash)) {
        // A valid in-progress fresh-send receipt deliberately shares this
        // exact path and will be replaced by the final receipt. Any other
        // malformed exact-path record is ambiguous continuity evidence: do
        // not silently resend work as though the file did not exist.
        const validFresh = validFreshDeliveryReceipt(
          record, cursorKey, sessionId, clientMessageId, requestHash,
        );
        if (file === scopedFile && !validFresh) {
          throw new Error("the exact durable turn receipt is malformed");
        }
        if (validFresh) continue;
        dbg("ignoring mismatched completed-turn receipt", "session=" + sessionId, "clientMessageId=" + clientMessageId);
        continue;
      }
      completedTurnReceipts.set(file, record);
      return record;
    } catch (error) {
      if (error && error.code === "ENOENT") continue;
      if (file === scopedFile) {
        throw new Error(`cannot validate exact durable turn receipt: ${(error && error.message) || String(error)}`);
      }
      dbg("legacy completed-turn receipt unavailable; ignoring unscoped compatibility record",
        "session=" + sessionId, "clientMessageId=" + clientMessageId,
        (error && error.message) || String(error));
    }
  }
  return null;
}

function writeCompletedTurnReceipt(
  cursorKey,
  sessionId,
  clientMessageId,
  requestHash,
  events,
  usage,
  {
    generation = 1,
    replace = false,
    requestKind = "continuation",
    clientLeaseToken = "",
    identityPolicy = TURN_IDENTITY_POLICY.LEGACY_CLIENT_MESSAGE_V1,
  } = {},
) {
  if (!clientMessageId) return null;
  const replayEvents = canonicalReplayEvents(events);
  const record = {
    version: TURN_RECEIPT_VERSION,
    keyFingerprint: keyFingerprint(cursorKey),
    sessionId: String(sessionId || ""),
    clientMessageId: String(clientMessageId),
    requestHash: String(requestHash || ""),
    generation: Number.isSafeInteger(generation) && generation >= 1 ? generation : 1,
    requestKind: requestKind === "fresh" ? "fresh" : "continuation",
    identityPolicy,
    clientLeaseToken: normalizeClientLeaseToken(clientLeaseToken),
    acceptancePhase: ACCEPTANCE_PHASE.COMPLETED,
    status: "completed",
    events: replayEvents,
    usage: usage && typeof usage === "object" && !Array.isArray(usage)
      ? JSON.parse(JSON.stringify(usage))
      : {},
    completedAt: nowMs(),
  };
  if (!validCompletedTurnReceipt(record, cursorKey, sessionId, clientMessageId, requestHash)) {
    throw new Error("completed-turn receipt exceeds the durable replay contract");
  }
  mkdirSync(COMPLETED_TURN_DIR, { recursive: true, mode: 0o700 });
  const file = completedTurnReceiptPath(cursorKey, sessionId, clientMessageId, requestHash);
  const committed = mutateTurnReceipt(file, (raw) => {
    completedTurnReceipts.delete(file);
    if (raw && validCompletedTurnReceipt(raw, cursorKey, sessionId, clientMessageId, requestHash)) {
      if (!replace || (raw.generation || 1) >= record.generation) return { commit: false, record: raw };
    }
    if (raw && !validFreshDeliveryReceipt(raw, cursorKey, sessionId, clientMessageId, requestHash)
        && !validCompletedTurnReceipt(raw, cursorKey, sessionId, clientMessageId, requestHash)) {
      throw new Error("cannot replace malformed durable turn receipt");
    }
    return { commit: true, record };
  });
  completedTurnReceipts.set(file, committed);
  releaseUnresolvedReceipt(file);
  return committed;
}

function validFreshDeliveryReceipt(record, cursorKey, sessionId, clientMessageId, requestHash = "") {
  const legacyV3 = record && record.version === 3 && record.status === "delivering";
  const legacyV45 = record && (record.version === 4 || record.version === 5)
    && Object.values(FRESH_ATTEMPT_STATE).includes(record.status);
  const phase = record ? resolveAcceptancePhase(record) : ACCEPTANCE_PHASE.NOT_SENT;
  const v6 = record && record.version === TURN_RECEIPT_VERSION
    && isAcceptancePhase(record.acceptancePhase)
    && record.acceptancePhase !== ACCEPTANCE_PHASE.COMPLETED
    && record.acceptancePhase !== ACCEPTANCE_PHASE.NOT_SENT;
  return !!record && typeof record === "object" && !Array.isArray(record)
    && (legacyV3 || legacyV45 || v6)
    && record.requestKind === "fresh"
    && record.keyFingerprint === keyFingerprint(cursorKey)
    && record.sessionId === String(sessionId || "")
    && record.clientMessageId === String(clientMessageId || "")
    && /^[a-f0-9]{64}$/.test(record.requestHash)
    && (!requestHash || record.requestHash === requestHash)
    && Number.isSafeInteger(record.generation) && record.generation >= 1
    && typeof record.agentId === "string" && record.agentId
    && typeof record.deliveryIdempotencyKey === "string" && record.deliveryIdempotencyKey
    && Object.prototype.hasOwnProperty.call(record, "deliveryMessage")
    && Array.isArray(record.deliveryAdvertise)
    && (record.deliverySystemBlockIds === undefined || validSystemBlockIds(record.deliverySystemBlockIds))
    && (record.version === 3
      || record.identityPolicy === TURN_IDENTITY_POLICY.INVOCATION_V1
      || record.identityPolicy === TURN_IDENTITY_POLICY.LEGACY_CLIENT_MESSAGE_V1)
    && (record.status !== FRESH_ATTEMPT_STATE.FAILED
      || (typeof record.failure === "string" && record.failure && Number.isFinite(record.failedAt)))
    && (phase !== ACCEPTANCE_PHASE.REJECTED_BEFORE_SEND
      || (typeof record.rejectionReason === "string" && record.rejectionReason && Number.isFinite(record.rejectedAt)));
}

function unresolvedFreshDeliveryReceipt(record) {
  return isUnresolvedAcceptanceRecord(record);
}

function readFreshDeliveryReceipt(cursorKey, sessionId, clientMessageId, requestHash = "") {
  if (!clientMessageId || !requestHash) return null;
  const file = completedTurnReceiptPath(cursorKey, sessionId, clientMessageId, requestHash);
  const cached = completedTurnReceipts.get(file);
  if (cached && validFreshDeliveryReceipt(cached, cursorKey, sessionId, clientMessageId, requestHash)) {
    return cached;
  }
  try {
    const record = readTurnReceiptFile(file);
    if (!validFreshDeliveryReceipt(record, cursorKey, sessionId, clientMessageId, requestHash)) {
      // A completed receipt at the same path is handled by the completed
      // reader. Any other existing record means we cannot prove whether the
      // SDK accepted the send, so fail closed instead of duplicating it.
      if (validCompletedTurnReceipt(record, cursorKey, sessionId, clientMessageId, requestHash)) return null;
      throw new Error("the exact durable fresh-delivery receipt is malformed");
    }
    completedTurnReceipts.set(file, record);
    return record;
  } catch (error) {
    if (error && error.code === "ENOENT") return null;
    throw new Error(`cannot validate exact durable fresh-delivery receipt: ${(error && error.message) || String(error)}`);
  }
}

function writeFreshDeliveryReceipt(cursorKey, sessionId, clientMessageId, requestHash, {
  generation,
  agentId,
  idempotencyKey,
  message,
  advertise,
  model,
  toolChoice,
  seededSystem,
  systemBlockIds: deliveredSystemBlockIds,
  hasImages,
  identityPolicy = TURN_IDENTITY_POLICY.LEGACY_CLIENT_MESSAGE_V1,
}) {
  // Persist the exact frozen envelope as PREPARED_DURABLE. The send boundary
  // is crossed only after a later fsynced MAYBE_ACCEPTED transition.
  const draft = {
    version: TURN_RECEIPT_VERSION,
    keyFingerprint: keyFingerprint(cursorKey),
    sessionId: String(sessionId || ""),
    clientMessageId: String(clientMessageId || ""),
    requestHash: String(requestHash || ""),
    generation: Number.isSafeInteger(generation) && generation >= 1 ? generation : 1,
    requestKind: "fresh",
    identityPolicy,
    agentId: String(agentId || ""),
    deliveryIdempotencyKey: String(idempotencyKey || ""),
    deliveryMessage: JSON.parse(canonicalJSONString(message)),
    deliveryAdvertise: JSON.parse(canonicalJSONString(Array.isArray(advertise) ? advertise : [])),
    deliveryModel: String(model || ""),
    deliveryToolChoice: String(toolChoice || ""),
    deliverySeededSystem: String(seededSystem || ""),
    deliverySystemBlockIds: validSystemBlockIds(deliveredSystemBlockIds)
      ? [...deliveredSystemBlockIds] : [],
    deliveryHasImages: hasImages === true,
    startedAt: nowMs(),
  };
  const record = applyAcceptanceTransition(draft, ACCEPTANCE_PHASE.PREPARED_DURABLE, { nowMs: nowMs() });
  if (!validFreshDeliveryReceipt(record, cursorKey, sessionId, clientMessageId, requestHash)) {
    throw new Error("fresh delivery receipt violates the durable send contract");
  }
  const bytes = Buffer.from(JSON.stringify(record) + "\n", "utf8");
  if (bytes.length > MAX_AGENT_TURN_BYTES * 2) {
    throw new Error("fresh delivery receipt exceeds twice the bounded request size");
  }
  mkdirSync(COMPLETED_TURN_DIR, { recursive: true, mode: 0o700 });
  const file = completedTurnReceiptPath(cursorKey, sessionId, clientMessageId, requestHash);
  reserveUnresolvedReceipt(file, bytes.length);
  const committed = mutateTurnReceipt(file, (raw) => {
    completedTurnReceipts.delete(file);
    const existingFresh = raw && validFreshDeliveryReceipt(raw, cursorKey, sessionId, clientMessageId, requestHash);
    const existingCompleted = raw && validCompletedTurnReceipt(raw, cursorKey, sessionId, clientMessageId, requestHash);
    if (existingFresh || existingCompleted) {
      throw new ToolRoundError(
        "fresh_delivery_ownership_conflict",
        "another bridge process already owns or completed this exact fresh send; retry the identical request",
        503,
      );
    }
    if (raw) throw new Error("cannot replace malformed durable turn receipt");
    return { commit: true, record };
  });
  completedTurnReceipts.set(file, committed);
  resizeUnresolvedReceipt(file, bytes.length);
  return committed;
}

function transitionAcceptancePhase(
  cursorKey,
  sessionId,
  clientMessageId,
  requestHash,
  nextPhase,
  details = {},
) {
  if (!clientMessageId || !requestHash || !isAcceptancePhase(nextPhase)) return null;
  const file = completedTurnReceiptPath(cursorKey, sessionId, clientMessageId, requestHash);
  const committed = mutateTurnReceipt(file, (raw) => {
    completedTurnReceipts.delete(file);
    if (raw && validCompletedTurnReceipt(raw, cursorKey, sessionId, clientMessageId, requestHash)) {
      return { commit: false, record: raw };
    }
    const existing = raw && validFreshDeliveryReceipt(raw, cursorKey, sessionId, clientMessageId, requestHash)
      ? raw : null;
    if (!existing) return { commit: false, record: raw };
    const from = resolveAcceptancePhase(existing);
    if (from === nextPhase && nextPhase === ACCEPTANCE_PHASE.ACCEPTED) {
      const record = {
        ...existing,
        version: TURN_RECEIPT_VERSION,
        acceptancePhase: ACCEPTANCE_PHASE.ACCEPTED,
        identityPolicy: existing.identityPolicy || TURN_IDENTITY_POLICY.LEGACY_CLIENT_MESSAGE_V1,
        acceptedAt: existing.acceptedAt || nowMs(),
      };
      if (details.evidence && !record.acceptanceEvidence) {
        record.acceptanceEvidence = String(details.evidence);
      }
      delete record.status;
      delete record.failedAt;
      delete record.failure;
      delete record.runningAt;
      return { commit: true, record };
    }
    const record = applyAcceptanceTransition(existing, nextPhase, {
      ...details,
      nowMs: nowMs(),
      envelope: existing,
    });
    record.identityPolicy = existing.identityPolicy || TURN_IDENTITY_POLICY.LEGACY_CLIENT_MESSAGE_V1;
    if (nextPhase !== ACCEPTANCE_PHASE.COMPLETED
        && !validFreshDeliveryReceipt(record, cursorKey, sessionId, clientMessageId, requestHash)) {
      throw new Error(`acceptance phase ${nextPhase} transition violates the durable receipt contract`);
    }
    return { commit: true, record };
  });
  if (committed) completedTurnReceipts.set(file, committed);
  return committed;
}

/** Dual-read shim: legacy running maps to ACCEPTED; failed writes are refused. */
function transitionFreshAttemptState(
  cursorKey,
  sessionId,
  clientMessageId,
  requestHash,
  state,
  details = {},
) {
  if (state === FRESH_ATTEMPT_STATE.RUNNING || state === ACCEPTANCE_PHASE.ACCEPTED) {
    return transitionAcceptancePhase(
      cursorKey, sessionId, clientMessageId, requestHash,
      ACCEPTANCE_PHASE.ACCEPTED, { ...details, evidence: details.evidence || "agent_send_resolved" },
    );
  }
  if (state === ACCEPTANCE_PHASE.MAYBE_ACCEPTED
      || state === ACCEPTANCE_PHASE.PREPARED_DURABLE
      || state === ACCEPTANCE_PHASE.REJECTED_BEFORE_SEND
      || state === ACCEPTANCE_PHASE.COMPLETED) {
    return transitionAcceptancePhase(cursorKey, sessionId, clientMessageId, requestHash, state, details);
  }
  if (state === FRESH_ATTEMPT_STATE.FAILED) {
    throw new IllegalAcceptanceTransition(
      resolveAcceptancePhase(readFreshDeliveryReceipt(cursorKey, sessionId, clientMessageId, requestHash)),
      ACCEPTANCE_PHASE.REJECTED_BEFORE_SEND,
    );
  }
  return null;
}

function cleanupCompletedTurnReceipts({ ttlMs = TERMINAL_ROUND_TTL_MS, maxTerminal = TERMINAL_ROUND_MAX } = {}) {
  let entries;
  try {
    entries = readdirSync(COMPLETED_TURN_DIR)
      .filter((name) => /^[a-f0-9]{64}\.json$/.test(name))
      .map((name) => {
        const file = path.join(COMPLETED_TURN_DIR, name);
        let status = "";
        let acceptancePhase = "";
        try {
          const raw = JSON.parse(readFileSync(file, "utf8"));
          status = raw.status || "";
          acceptancePhase = raw.acceptancePhase || "";
        } catch {}
        return { file, mtimeMs: statSync(file).mtimeMs, status, acceptancePhase };
      })
      .sort((a, b) => b.mtimeMs - a.mtimeMs);
  } catch (error) {
    if (error && error.code === "ENOENT") return;
    dbg("completed-turn receipt cleanup skipped", (error && error.message) || String(error));
    return;
  }
  const cutoff = ttlMs > 0 ? nowMs() - ttlMs : Number.NEGATIVE_INFINITY;
  let terminalIndex = 0;
  for (let i = 0; i < entries.length; i++) {
    // Unresolved acceptance evidence (v6 phases + legacy statuses) must never
    // be TTL-evicted: it is the only proof a send may have crossed its boundary.
    if (entries[i].status === "expired") continue;
    const phase = isAcceptancePhase(entries[i].acceptancePhase)
      ? entries[i].acceptancePhase
      : resolveAcceptancePhase({ status: entries[i].status, acceptancePhase: entries[i].acceptancePhase });
    if (isUnresolvedAcceptancePhase(phase)
        || entries[i].status === "delivering"
        || entries[i].status === FRESH_ATTEMPT_STATE.UNKNOWN
        || entries[i].status === FRESH_ATTEMPT_STATE.RUNNING
        || entries[i].status === FRESH_ATTEMPT_STATE.FAILED
        || phase === ACCEPTANCE_PHASE.REJECTED_BEFORE_SEND) continue;
    if ((ttlMs > 0 && entries[i].mtimeMs < cutoff)
        || (maxTerminal >= 0 && terminalIndex >= maxTerminal)) {
      try {
        const state = receiptCAS.readRecover(entries[i].file);
        if (resolveAcceptancePhase(state.record) !== ACCEPTANCE_PHASE.COMPLETED
            && state.record?.status !== "completed") continue;
        const tombstone = receiptCAS.commit(entries[i].file, state, {
          version: TURN_RECEIPT_VERSION,
          status: "expired",
          expiredAt: nowMs(),
        }).record;
        receiptCAS.cleanupArtifacts(entries[i].file, { keepRevision: tombstone.revision });
        completedTurnReceipts.delete(entries[i].file);
      } catch (error) {
        if (!(error instanceof DurableCASConflict)) {
          dbg("completed-turn receipt cleanup skipped a racing record", (error && error.message) || String(error));
        }
      }
    }
    terminalIndex++;
  }
}

function sdkAgentGCRoots() {
  const roots = new Set();
  for (const session of sessions.values()) {
    if (session?.agentId) roots.add(session.agentId);
    if (session?.pendingRecoveryAlias) roots.add(session.pendingRecoveryAlias);
  }
  try {
    for (const name of readdirSync(AGENT_ALIAS_DIR)) {
      if (!name.endsWith(".json")) continue;
      try {
        const record = JSON.parse(readFileSync(path.join(AGENT_ALIAS_DIR, name), "utf8"));
        if (typeof record?.agentId === "string" && record.agentId) roots.add(record.agentId);
      } catch {}
    }
  } catch (error) {
    if (error?.code !== "ENOENT") throw error;
  }
  const { journal } = getRoundInfrastructure();
  for (const record of journal.records()) {
    const unresolvedDeferred = Array.isArray(record?.deferredInputs)
      && record.deferredInputs.some((item) => item && (
        item.state === DeferredInputState.QUEUED || item.state === DeferredInputState.DELIVERING
      ));
    if (record?.state !== RoundState.TERMINAL || unresolvedDeferred) {
      if (typeof record?.agentId === "string" && record.agentId) roots.add(record.agentId);
      if (typeof record?.recovery?.replacementAgentId === "string") roots.add(record.recovery.replacementAgentId);
      for (const item of record?.deferredInputs || []) {
        if (typeof item?.deliveryAgentId === "string" && item.deliveryAgentId) roots.add(item.deliveryAgentId);
      }
    }
  }
  try {
    for (const name of readdirSync(COMPLETED_TURN_DIR)) {
      if (!/^[a-f0-9]{64}\.json$/.test(name)) continue;
      try {
        const record = receiptCAS.readRecover(path.join(COMPLETED_TURN_DIR, name)).record;
        if (isUnresolvedAcceptanceRecord(record)
            && typeof record?.agentId === "string" && record.agentId) roots.add(record.agentId);
      } catch {}
    }
  } catch (error) {
    if (error?.code !== "ENOENT") throw error;
  }
  return roots;
}

function sdkAgentLiveRoots(durableRoots) {
  const roots = new Set(durableRoots);
  for (const session of sessions.values()) {
    if (session?.agentId) roots.add(session.agentId);
    if (session?.pendingRecoveryAlias) roots.add(session.pendingRecoveryAlias);
  }
  for (const round of liveToolRounds.values()) {
    if (round?.agentId) roots.add(round.agentId);
  }
  return roots;
}

let sdkAgentGCRunning = false;
async function runSdkAgentMaintenance() {
  if (!SDK_AGENT_GC_ENABLED || sdkAgentGCRunning) return;
  sdkAgentGCRunning = true;
  try {
    for (const [scope, entry] of platforms.entries()) {
      const platform = await entry.promise;
      const durableRoots = sdkAgentGCRoots();
      const stats = await collectSdkAgents({
        platform,
        scope,
        quarantineRoot: SDK_AGENT_GC_DIR,
        protectedAgentIds: durableRoots,
        // Re-read shared durable roots before every mutation. Multiple bridge
        // processes may overlap during deploys, so the initial census is not
        // deletion authority.
        refreshProtectedAgentIds: () => sdkAgentLiveRoots(sdkAgentGCRoots()),
        minIdleMs: SDK_AGENT_GC_MIN_IDLE_MS,
        quarantineMs: SDK_AGENT_GC_QUARANTINE_MS,
        maxScan: SDK_AGENT_GC_MAX_SCAN,
        maxMutations: SDK_AGENT_GC_MAX_MUTATIONS,
      });
      if (stats.quarantined || stats.deleted || stats.restored || stats.skipped) {
        console.log("[cursor-agent-bridge] SDK agent GC", JSON.stringify({ scope, ...stats }));
      }
    }
  } catch (error) {
    console.error("[cursor-agent-bridge] SDK agent GC failed closed; preserving state:",
      (error && error.message) || String(error));
  } finally {
    sdkAgentGCRunning = false;
  }
}

function runDurableMaintenance() {
  const { journal } = getRoundInfrastructure();
  journal.cas.sweepDirectory(journal.dir, { orphanAgeMs: UNRESOLVED_RESERVATION_ORPHAN_MS });
  receiptCAS.sweepDirectory(COMPLETED_TURN_DIR, { orphanAgeMs: UNRESOLVED_RESERVATION_ORPHAN_MS });
  journal.cleanupTerminal({ ttlMs: TERMINAL_ROUND_TTL_MS, maxTerminal: TERMINAL_ROUND_MAX });
  cleanupCompletedTurnReceipts({ ttlMs: TERMINAL_ROUND_TTL_MS, maxTerminal: TERMINAL_ROUND_MAX });
  reconcileUnresolvedReservations();
  void runSdkAgentMaintenance();
}

function sdkUserTextHash(message) {
  const text = typeof message === "string" ? message : String(message && message.text || "");
  return createHash("sha256").update(text).digest("hex");
}

function sdkSendIdempotencyKey(session, clientMessageId, requestHash, deferred = null, generation = 1) {
  if (!clientMessageId) return "";
  if (typeof deferred?.deliveryIdempotencyKey === "string" && deferred.deliveryIdempotencyKey) {
    return deferred.deliveryIdempotencyKey;
  }
  // Records written before scoped send keys were introduced already used the
  // raw client message id. Preserve that exact key across an upgrade so an
  // uncertain in-flight send cannot be duplicated during migration.
  if ((deferred?.deliveryAttempts || 0) > 0) return clientMessageId;
  const agentScope = createHash("sha256")
    .update(keyFingerprint(session.cursorKey))
    .update("\0")
    .update(String(session.agentId || session.id || ""))
    .digest("base64url")
    .slice(0, 24);
  const inputScope = /^[a-f0-9]{64}$/.test(requestHash) ? requestHash.slice(0, 24) : "unknown_input";
  const turnGeneration = Number.isSafeInteger(generation) && generation >= 1 ? generation : 1;
  return `ccsend2_${agentScope}_${inputScope}_g${turnGeneration}_${clientMessageId}`;
}

function canonicalAttemptSignature(value) {
  const ordered = (input) => {
    if (Array.isArray(input)) return input.map(ordered);
    if (input && typeof input === "object") {
      return Object.fromEntries(Object.keys(input).sort().map((key) => [key, ordered(input[key])]));
    }
    return input;
  };
  return JSON.stringify(ordered(value)) ?? String(value);
}

function clientToolDescriptorFingerprint(descriptor) {
  return descriptor
    ? createHash("sha256").update(canonicalAttemptSignature(descriptor)).digest("hex")
    : null;
}

// Hidden SDK preflight failures never receive signed executable ids, but they
// still need a durable audit trail. Persist only schema paths, keywords,
// transform kinds, hashes, and tool metadata—never raw argument values.
function recordSanitizedInternalAttempt(session, {
  kind,
  toolName = "",
  detail = "",
  errors = [],
  transforms = [],
  input = undefined,
}) {
  const normalizedErrors = (Array.isArray(errors) ? errors : []).slice(0, 64).map((error) => ({
    path: String(error && error.path || "arguments").slice(0, 512),
    keyword: String(error && error.keyword || "contract").slice(0, 128),
  }));
  const transformKinds = (Array.isArray(transforms) ? transforms : []).slice(0, 64).map((transform) =>
    String(transform && (transform.type || transform.kind || transform.action) || "transform").slice(0, 128));
  const descriptor = session && session.toolRegistry && session.toolRegistry.find(toolName);
  const schema = descriptor && descriptor.inputSchema;
  const schemaFingerprint = schema ? createHash("sha256").update(safeJson(schema)).digest("hex") : null;
  const argumentFingerprint = input === undefined
    ? null
    : createHash("sha256").update(canonicalAttemptSignature(input)).digest("hex");
  const signatureSource = canonicalAttemptSignature({
    kind,
    toolName,
    detail,
    errors: normalizedErrors,
    transformKinds,
    schemaFingerprint,
    argumentFingerprint,
    toolInventoryEpoch: session && session.toolInventoryEpoch || null,
  });
  const record = {
    attemptId: randomUUID(),
    at: nowMs(),
    sessionIdHash: createHash("sha256").update(String(session && session.id || "")).digest("hex"),
    canonicalToolName: String(toolName || "").slice(0, 512),
    category: String(kind || "contract").slice(0, 128),
    schemaFingerprint,
    argumentFingerprint,
    toolInventoryEpoch: session && session.toolInventoryEpoch || null,
    errorSignature: createHash("sha256").update(signatureSource).digest("hex"),
    errors: normalizedErrors,
    transforms: transformKinds,
    executed: false,
  };
  const dir = STATE_ROOT;
  const file = path.join(dir, "internal-tool-attempts.jsonl");
  const previous = `${file}.previous`;
  try {
    mkdirSync(dir, { recursive: true, mode: 0o700 });
    try {
      if (statSync(file).size >= INTERNAL_ATTEMPT_MAX_BYTES) {
        rmSync(previous, { force: true });
        renameSync(file, previous);
      }
    } catch (error) {
      if (!error || error.code !== "ENOENT") throw error;
    }
    const fd = openSync(file, "a", 0o600);
    try {
      writeSync(fd, `${JSON.stringify(record)}\n`);
      fsyncSync(fd);
    } finally {
      closeSync(fd);
    }
  } catch (error) {
    // Diagnostics cannot make a valid turn unavailable.
    dbg("failed to persist sanitized internal tool attempt", "session=" + (session && session.id),
      "kind=" + kind, (error && error.message) || String(error));
  }
  return record;
}

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

// isUpstreamUnauthenticated detects Cursor rejecting the bridge's auth on the cached connection. The crsr_ key
// mints a session token; when it goes stale/invalid mid-connection Cursor RST_STREAMs the run with a @connectrpc
// [unauthenticated] ConnectError on the stream trailer. Like the rate-limit poison, the failure is bound to the
// ONE persistent HTTP/2 connection (the getPlatform cache), so EVERY reused stream on it then fails the same way
// until the platform is recycled and re-dialed — which re-mints auth. getPlatform only evicts a REJECTED create,
// never a live-but-poisoned one, so without this the connection wedges every turn into a 500 until restart.
// Exported for tests.
function isUpstreamUnauthenticated(reason) {
  if (!reason) return false;
  if (reason.code === "unauthenticated" || reason.code === "unauthorized") return true;
  const msg = (reason.message != null ? String(reason.message) : (typeof reason === "string" ? reason : ""));
  return /\bunauthenticated\b|\bunauthorized\b/i.test(msg);
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

function terminalizePoisonedPlatformSessions(h, reason) {
  let affected = 0;
  for (const session of sessions.values()) {
    if (!session || keyHash(session.cursorKey) !== h || session.done) continue;
    affected++;
    const error = reason instanceof Error ? reason : new Error(String(reason || "upstream connection poisoned"));
    if (session.run || session.sendPending) {
      // onRunError emits the typed terminal, transitions a versioned fresh
      // attempt to FAILED, releases FIFO waiters, and discards the agent.
      session.abortFromSdk(error);
      continue;
    }
    // A paused callback owns durable obligations even without an active HTTP
    // response. Terminal-journal those obligations before recycling the store.
    cancelSessionDetached(session, {
      terminalReason: TerminalReason.RUN_ERROR,
      detail: error.message,
    });
  }
  return affected;
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

class AdvertisedToolRegistry extends ToolContractRegistry {
  scoped(toolChoice) { return effectiveAdvertise(this.tools, toolChoice); }
}

class Session {
  constructor(id, cursorKey) {
    this.id = id;
    this.cursorKey = cursorKey || API_KEY; // the Cursor key whose platform this session runs on
    const durableAlias = readDurableAgentAlias(this.cursorKey, id);
    // C05: the DURABLE Cursor agent id is DECOUPLED from the external sessionID. `id` is the stable routing
    // key the Go executor derives (continuations keep routing here); `agentId` is what we hand to
    // resumeAgent/createAgent. They start equal, but on ERROR_CONVERSATION_TOO_LONG we ROTATE `agentId`
    // (e.g. <id>_r2) and tombstone the poisoned durable agent, so the next turn seeds a FRESH agent under a
    // new id and never resumeAgent()s the over-budget one — while the external sessionID is unchanged.
    this.agentId = durableAlias?.agentId || `${id}_${DURABLE_AGENT_CONTEXT_EPOCH}`;
    this.recoveryEpoch = durableAlias?.recoveryEpoch || 0; // C05: increments each too-long rotation
    this.modelEpoch = durableAlias?.modelEpoch || 0; // ADD-62: increments each MODEL-CHANGE rotation; a SEPARATE budget from
                                  // recoveryEpoch so toggling models never burns the crash-recovery rotations
    this.keyEpoch = durableAlias?.keyEpoch || 0; // ADD-79: increments each CURSOR-KEY-CHANGE rotation; a SEPARATE budget so a
                                  // key rotation never burns the crash-recovery / model-change rotations. A turn
                                  // whose upstream Cursor key fingerprint differs from this session's tombstones
                                  // the durable agent (bound to the old account) + seeds a fresh agent under the
                                  // new key, instead of silently continuing on the old (possibly revoked) account.
    this.contextEpoch = durableAlias?.contextEpoch || 0; // System-context replacement/removal/reorder rotation.
    this.seededSystemBlockIds = Array.isArray(durableAlias?.systemBlockIds)
      ? [...durableAlias.systemBlockIds] : [];
    this.model = null;            // ADD-62: the model the durable agent was created/resumed under. A turn that
                                  // requests a DIFFERENT model rotates the durable agent (the old agent is bound
                                  // to the old model) + forces a re-seed, instead of silently answering from it.
    this.agent = null; this.agentPromise = null; this.run = null;
    this.utilityOneShot = false;
    // A restart-recovery target is not the durable active alias until its
    // first SDK send resolves. Keeping this intent process-local is safe: the
    // ToolRound journal deterministically reconstructs the same target after
    // a crash, while the prior alias continues to name a real seeded agent.
    this.pendingRecoveryAlias = "";
    this.cancelPromise = null; this.cancelNotifyRequested = false;
    this.activeRes = null; this.responseWriter = null;
    this.sendPending = false;
    this.activeClientMessageId = "";
    this.activeClientMessageHash = "";
    this.activeClientMessageGeneration = 1;
    this.activeClientMessageKind = "";
    this.activeIdentityPolicy = TURN_IDENTITY_POLICY.NONE;
    this.activeDeferredInputId = "";
    // Opaque Go fresh-turn lease owner. It is never interpreted by the
    // bridge: ToolRound journals preserve it and terminal SSE frames echo it
    // so a signed /continue can close exactly the lease that opened the SDK
    // callback, even after a sidecar-only restart.
    this.clientLeaseToken = "";
    this.currentRound = null; this.roundSeq = 0;
    // When a continuation lands after the paused callback process is gone, the
    // replacement run keeps this terminal source round attached until it
    // reaches a real final terminal. That lets the journal distinguish
    // "reseed started" from "reseed completed" and makes a lost-response retry
    // idempotent without inventing another SDK run.
    this.recoverySourceRound = null;
    this.roundRecoveryContext = null; // bounded seed persisted when each ToolRound opens
    this.flushTimer = null;
    this.stepToolStarted = 0;     // tool-call-started deltas seen for the CURRENT assistant step (the step's tool
                                  // count); used to wait for a slow batch before pausing. Reset at step/turn boundaries.
    this.batchWaitExtensions = 0; // how many extra debounce windows we have waited for the current step's batch
                                  // (bounded by TOOL_BATCH_MAX_EXTENSIONS so an over-count never hangs the turn).
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
                                  // tool calls are journaled by ToolRound). Cleared at clearTurnState.
    this.pendingDeltaBytes = 0;   // running byte size of pendingDeltas (bounded like outQueue)
    // Ordered frames emitted for the CURRENT HTTP/request identity. The final
    // completion receipt durably commits this segment plus its terminal before
    // a clean end_turn is allowed onto the wire. Tool pauses start a new
    // segment on the following /continue, so replay never re-emits tools from
    // an earlier already-receipted request.
    this.replayEvents = [];
    this.replayEventBytes = 0;
    this.replayReservationBytes = 0;
    this.replayMemory = null;

    this.replayEventOverflow = false;
    this.replaySegmentIdentity = "";
    // stream-resume-v1: durable journal-before-expose. Legacy clients leave
    // these unset and keep the existing immediate writePayload path.
    this.streamResumeEnabled = false;
    this.invocationId = "";
    this.streamJournal = createStreamJournalFromEnv() || null;
    this.streamJournalPhase = "accepted";
    this._exposeChain = Promise.resolve();

    this.toolRegistry = new AdvertisedToolRegistry();
    this.toolInventoryEpoch = null;
    // Exact MCP server keys passed to the current SDK agent handle. The
    // generic key is always included and absorbs tools whose natural server
    // appears only after the durable agent's fixed mcpServers map was built.
    this.mcpServerKeys = null;
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
    // Turn-scoped defense against Cursor's internal large-result artifact
    // workflow. Only hashes and rejected paths are retained; tool content
    // remains owned by the durable ToolRound receipt and SDK continuation.
    this.syntheticArtifactUserText = "";
    this.syntheticArtifactResults = [];
    this.syntheticArtifactBlockedPaths = new Set();
    this.syntheticArtifactRejects = 0;
    this.lastActivity = nowMs();
    this.lastSdkActivityAt = this.lastActivity;
    this.lastStallLogAt = 0;
    this.done = false;
    this.tail = Promise.resolve();   // per-session FIFO chain: each new-user turn runs after the prior one's run completes
    this.waiters = 0;                // new-user turns queued but not yet running (single source of truth for depth-cap + eviction safety)
    this.writeFailed = false;
    this._logicalDone = [];          // resolvers fired when the live run TRULY completes (onRunComplete/onRunError/cancel), NOT at a tool-pause
    this._turnDone = [];
    this.lastSettledTurnToken = 0;
    this.lastTerminalTurnToken = -1; // prevents cancellation/error cleanup from emitting a second terminal
    this.runEpoch = 0;               // bumped per run + on cancel; a run.wait() callback ignores its result if the epoch advanced (the run was superseded/cancelled and a new turn may already own the session)
    // ADD-106 (Comment 3): per-LOGICAL-RUN agentic-loop bound counters. Reset by resetLoopBounds() when a fresh
    // send starts a new logical run; advanced once per tool-result round (pauseForTools). loopTripped latches so
    // the bound is enforced exactly once per run (the run is terminated as turn_end{error}, never a clean end).
    this.toolRounds = 0;             // tool-result rounds taken by the CURRENT logical run (pauseForTools cycles)
    this.lastToolSig = null;         // signature (name+args) of the SOLE tool in the last single-tool round, or null
    this.repeatToolCount = 0;        // consecutive single-tool rounds whose signature equals lastToolSig
    this.invalidToolCalls = 0;       // schema-rejected calls handled inside the bridge (no client round allocated)
    this.internalRejectionCounts = new Map();
    this.internalRejectionSignatures = new Map();
    this.loopTripped = false;        // latched true once a loop bound trips, so we terminate the run only once
  }

  get advertise() { return this.toolRegistry.all(); }
  set advertise(tools) { this.toolRegistry.replace(tools); }

  touch() { this.lastActivity = nowMs(); }
  beginReplaySegment(identity = "") {
    const nextIdentity = String(identity || "");
    const reuseReservation = nextIdentity && nextIdentity === this.replaySegmentIdentity
      && this.replayMemory && this.replayMemory.reserved > 0;
    if (!reuseReservation) {
      if (this.replayMemory) this.replayMemory.release();
      this.replayMemory = null;
      this.replayReservationBytes = 0;
    }
    if (nextIdentity && !reuseReservation) {
      this.replayMemory = new AdaptiveMemoryBudget({
        limit: replayMemoryBudget.limit,
        initial: Math.min(REPLAY_MEMORY_INITIAL_BYTES, MAX_AGENT_TURN_BYTES),
        hardCap: MAX_AGENT_TURN_BYTES,
        shared: replayMemoryBudget,
      });
      try {
        this.replayMemory.ensure(1);
      } catch (error) {
        this.replayMemory = null;
        throw new ToolRoundError(
          error.code || "local_replay_capacity",
          error.message || "process-wide durable replay memory capacity is occupied; retry this turn shortly",
          error.status || 503,
        );
      }
      this.replayReservationBytes = this.replayMemory.reserved;
    }
    this.replayEvents = [];
    this.replayEventBytes = 0;
    this.replayEventOverflow = false;
    this.replaySegmentIdentity = nextIdentity;
  }
  recordReplayEvent(event) {
    try {
      const canonical = canonicalReplayEvent(event);
      if (canonical.type === "turn_end") return false;
      const bytes = Buffer.byteLength(canonicalJSONString(canonical), "utf8") + 1;
      const needed = this.replayEventBytes + bytes;
      if (this.replayMemory) {
        try {
          this.replayMemory.ensure(needed);
          this.replayReservationBytes = this.replayMemory.reserved;
        } catch (error) {
          this.replayEventOverflow = true;
          return false;
        }
      } else if (needed > MAX_AGENT_TURN_BYTES) {
        this.replayEventOverflow = true;
        return false;
      }
      this.replayEvents.push(canonical);
      this.replayEventBytes += bytes;
      return true;
    } catch (error) {
      this.replayEventOverflow = true;
      dbg("completed replay event rejected", "session=" + this.id,
        (error && error.message) || String(error));
      return false;
    }
  }

  completedReplayEvents(terminal) {
    if (this.replayEventOverflow) throw new Error("completed replay event log overflowed its durable bound");
    return canonicalReplayEvents([...this.replayEvents, terminal]);
  }
  resetSyntheticArtifactGuard(userText = "") {
    this.syntheticArtifactUserText = typeof userText === "string" ? userText : "";
    this.syntheticArtifactResults = [];
    this.syntheticArtifactBlockedPaths.clear();
    this.syntheticArtifactRejects = 0;
  }
  rememberLargeMcpResult(content, toolName = "") {
    const text = typeof content === "string" ? content : JSON.stringify(content ?? "");
    const hashes = artifactContentHashes(text);
    if (hashes.length === 0) return false;
    this.syntheticArtifactResults.push({ hashes: new Set(hashes), bytes: Buffer.byteLength(text), toolName: String(toolName || "") });
    if (this.syntheticArtifactResults.length > SYNTHETIC_ARTIFACT_RESULT_WINDOW) this.syntheticArtifactResults.shift();
    return true;
  }
  userExplicitlyRequestedPath(candidate) {
    return typeof candidate === "string" && candidate.length > 0 && this.syntheticArtifactUserText.includes(candidate);
  }
  syntheticArtifactDecision(name, input) {
    const family = clientToolFamily(name);
    if (!SYNTHETIC_AGENT_ARTIFACT_FAMILIES.has(family)) return null;
    const paths = artifactInputPaths(input);
    const command = artifactInputText(input, SYNTHETIC_AGENT_ARTIFACT_COMMAND_KEYS);
    const explicit = paths.some((candidate) => this.userExplicitlyRequestedPath(candidate));
    let reason = "";

    if (!explicit && syntheticAgentArtifactRequest(name, input)) {
      reason = "reserved_agent_tools_path";
    }
    if (!reason && !explicit && family === "write") {
      const content = artifactInputText(input, SYNTHETIC_AGENT_ARTIFACT_CONTENT_KEYS);
      const hashes = artifactContentHashes(content);
      if (hashes.some((hash) => this.syntheticArtifactResults.some((result) => result.hashes.has(hash)))) {
        reason = "copied_large_mcp_result";
      }
    }
    if (!reason) {
      const referencesRejectedPath = [...this.syntheticArtifactBlockedPaths].some((blockedPath) =>
        paths.includes(blockedPath) || command.includes(blockedPath));
      if (referencesRejectedPath) reason = "rejected_artifact_followup";
    }
    if (!reason) return null;

    for (const candidate of paths) this.syntheticArtifactBlockedPaths.add(candidate);
    this.syntheticArtifactRejects++;
    return { reason, attempt: this.syntheticArtifactRejects, paths };
  }
  syntheticArtifactFailure(name, input) {
    const decision = this.syntheticArtifactDecision(name, input);
    return decision ? syntheticArtifactFailure(decision) : null;
  }
  ensureToolRound() {
    if (this.currentRound && this.currentRound.state !== RoundState.TERMINAL) return this.currentRound;
    const { journal, codec } = getRoundInfrastructure();
    this.currentRound = new ToolRound({
      sessionId: this.id,
      agentId: this.agentId,
      runEpoch: this.runEpoch,
      roundSeq: ++this.roundSeq,
      tenantFingerprint: keyFingerprint(this.cursorKey),
      model: this.model || "",
      clientLeaseToken: this.clientLeaseToken,
      recoveryContext: this.roundRecoveryContext,
      journal,
      codec,
      clock: nowMs,
      terminalCallback: (round) => {
        if (liveToolRounds.get(round.route) === round) liveToolRounds.delete(round.route);
      },
    });
    liveToolRounds.set(this.currentRound.route, this.currentRound);
    return this.currentRound;
  }
  pendingCount() { return this.currentRound ? this.currentRound.pendingCount : 0; }
  pendingIds() { return this.currentRound ? [...this.currentRound.callbacks.keys()] : []; }
  pendingHas(id) { return !!(this.currentRound && this.currentRound.callbacks.has(id)); }
  activeToolBatch() {
    if (!this.currentRound) return [];
    return this.currentRound.batch.map((id) => this.currentRound.calls.get(id)).filter(Boolean);
  }
  queuedToolCalls() {
    if (!this.currentRound) return [];
    return this.currentRound.outbound.map((id) => this.currentRound.calls.get(id)).filter(Boolean);
  }
  hasQueuedWaiters() { return this.waiters > 0; }
  resetSeedState() {
    this.seeded = false;
    this.seededSystem = "";
    this.seededSystemBlockIds = [];
    this.historyFingerprint = null;
  }
  persistDurableAlias() {
    writeDurableAgentAlias(this.cursorKey, this.id, this.agentId, this);
  }
  promotePendingRecoveryAlias() {
    if (!this.pendingRecoveryAlias) return;
    if (this.pendingRecoveryAlias !== this.agentId) {
      throw new Error("pending recovery alias no longer matches the active SDK agent");
    }
    this.persistDurableAlias();
    this.pendingRecoveryAlias = "";
  }
  async finishRotationCancel() { await this.cancel(); this.done = false; }
  whenLogicalDone() { if (!this.run && !this.sendPending) return Promise.resolve(); return new Promise((r) => this._logicalDone.push(r)); }
  notifyLogicalDone() { const ws = this._logicalDone; this._logicalDone = []; for (const w of ws) { try { w(); } catch {} } }
  whenTurnSettled(token = this.turnToken) {
    if (this.lastSettledTurnToken >= token) return Promise.resolve();
    return new Promise((resolve) => this._turnDone.push({ token, resolve }));
  }
  beginResponse(res) {
    this.activeRes = res;
    this.writeFailed = false;
    const writer = new SseWriter(res, {
      maxQueueBytes: COMPOSER_OUT_QUEUE_MAX_BYTES,
      onFailure: (error) => {
        if (this.responseWriter !== writer) return;
        this.writeFailed = true;
        this.lastRunError = `[bridge] SSE transport failure: ${error.message}`;
        if (this.currentRound && this.currentRound.state !== RoundState.TERMINAL) {
          try { this.currentRound.terminalize(TerminalReason.TRANSPORT_ERROR, error.message); } catch {}
        }
        this.settle();
        cancelSessionDetached(this, { terminalReason: TerminalReason.TRANSPORT_ERROR, detail: error.message });
      },
    });
    this.responseWriter = writer;
    return writer;
  }
  sse(obj) { return this.sseReceipt(obj).ok; }
  sseReceipt(obj) {
    let value = obj;
    if (obj && obj.type === "turn_end") {
      const { _clientLeaseTerminal, ...wire } = obj;
      const terminal = typeof _clientLeaseTerminal === "boolean"
        ? _clientLeaseTerminal : obj.stop_reason !== "tool_use";
      value = withClientLease(wire, this, terminal);
    }
    const journalTypes = obj && (obj.type === "text" || obj.type === "reasoning"
      || obj.type === "tool_call" || obj.type === "turn_end");
    if (this.streamResumeEnabled && this.invocationId && this.streamJournal && journalTypes) {
      return this.journaledSseReceipt(value, obj);
    }
    const receipt = this.writePayload(formatSseData(value));
    if (receipt.ok && obj && (obj.type === "text" || obj.type === "reasoning" || obj.type === "tool_call")) {
      this.recordReplayEvent(obj);
    }
    if (obj && obj.type === "turn_end" && receipt.ok) this.lastTerminalTurnToken = this.turnToken;
    return receipt;
  }
  journaledSseReceipt(value, original) {
    if (!this.responseWriter && this.activeRes) this.beginResponse(this.activeRes);
    if (!this.responseWriter || this.writeFailed) {
      const handedToNode = Promise.reject(new Error("stream dead"));
      handedToNode.catch(() => {});
      return { queued: false, handedToNode, ok: false };
    }
    const invocationId = this.invocationId;
    const journal = this.streamJournal;
    const phase = this.streamJournalPhase || "accepted";
    let resolveHanded;
    let rejectHanded;
    const handedToNode = new Promise((res, rej) => { resolveHanded = res; rejectHanded = rej; });
    handedToNode.catch(() => {});
    this._exposeChain = this._exposeChain.catch(() => {}).then(async () => {
      let committed;
      try {
        committed = await journal.appendBeforeExpose(invocationId, original.type, value, {
          acceptancePhase: phase,
        });
      } catch (error) {
        const wrapped = error instanceof Error ? error : new Error(String(error));
        this.failWrite(`stream journal commit failed: ${wrapped.message}`);
        rejectHanded(wrapped);
        throw wrapped;
      }
      const frame = formatSseFrame(value, {
        invocationId,
        sequence: committed.sequence,
      });
      const receipt = this.writePayload(frame);
      if (!receipt.ok) {
        const err = new Error("stream dead after journal commit");
        rejectHanded(err);
        throw err;
      }
      if (original && (original.type === "text" || original.type === "reasoning" || original.type === "tool_call")) {
        this.recordReplayEvent(original);
      }
      if (original && original.type === "turn_end") this.lastTerminalTurnToken = this.turnToken;
      try {
        await receipt.handedToNode;
        resolveHanded();
      } catch (error) {
        rejectHanded(error);
        throw error;
      }
      return receipt;
    });
    return { queued: true, handedToNode, ok: true };
  }
  writePayload(payload) {
    if (!this.responseWriter && this.activeRes) this.beginResponse(this.activeRes);
    if (!this.responseWriter || this.writeFailed) {
      const handedToNode = Promise.reject(new Error("stream dead"));
      handedToNode.catch(() => {});
      return { queued: false, handedToNode, ok: false };
    }
    const frameError = sseFrameSizeError(payload);
    if (frameError) {
      this.responseWriter.fail(frameError);
      const handedToNode = Promise.reject(frameError);
      handedToNode.catch(() => {});
      return { queued: false, handedToNode, ok: false };
    }
    return this.responseWriter.write(payload);
  }
  failWrite(reason) {
    if (this.responseWriter) this.responseWriter.fail(new Error(reason));
  }
  async finishResponse() {
    if (this.responseWriter) await this.responseWriter.endAfter("data: [DONE]\n\n");
  }
  detachDrain() {}

  normalizeClientToolInput(name, input) {
    const normalized = this.toolRegistry.normalize(name, input);
    if (name === "Workflow" && normalized.value && typeof normalized.value.script === "string") {
      const snapped = snapWorkflowAgentTypes(normalized.value.script);
      if (snapped !== normalized.value.script) normalized.value = { ...normalized.value, script: snapped };
    }
    return normalized;
  }

  settleAnnouncedToolAttempt() {
    const handed = this.activeToolBatch().length;
    if (this.stepToolStarted > handed) this.stepToolStarted--;
  }

  internalContractFailure(name, error) {
    return {
      content: `Client tool '${name}' was not executed because its arguments were ambiguous or malformed: ${error.message}`,
      isError: true,
      structuredContent: {
        code: error.code || "client_tool_normalization_failed",
        tool: name,
        path: error.path || "arguments",
        executed: false,
      },
    };
  }

  validateClientToolInput(name, input, transforms = [], { settleAnnounced = false } = {}) {
    const failure = this.toolRegistry.validate(name, input, transforms);
    if (!failure) return null;
    this.recordInternalToolRejection("schema", name, {
      settleAnnounced,
      toolName: name,
      errors: failure.structuredContent && failure.structuredContent.errors,
      transforms,
      input,
    });
    dbg("client tool schema preflight rejected before handoff", "session=" + this.id,
      "name=" + name, "invalidCalls=" + this.invalidToolCalls,
      "errors=" + safeJson(failure.structuredContent && failure.structuredContent.errors));
    if (this.loopTripped) {
      const reason = this.lastRunError || "client_tool_contract_mismatch: repeated invalid client-tool call";
      failure.content += `\n${reason}`;
      failure.structuredContent.limitExceeded = true;
    }
    return failure;
  }

  recordInternalToolRejection(kind, detail = "", {
    settleAnnounced = false,
    toolName = detail,
    errors = [],
    transforms = [],
    input = undefined,
  } = {}) {
    if (this.loopTripped) return true;
    if (settleAnnounced) this.settleAnnouncedToolAttempt();
    const attempt = recordSanitizedInternalAttempt(this, { kind, toolName, detail, errors, transforms, input });
    this.invalidToolCalls++;
    const categoryCount = (this.internalRejectionCounts.get(kind) || 0) + 1;
    this.internalRejectionCounts.set(kind, categoryCount);
    // A repetition is identical only when tool, schema epoch, normalized
    // error, transforms, and the privacy-safe argument fingerprint all match.
    // The old kind+detail signature collapsed every `_grep:ambiguous_alias`
    // correction into one bucket and killed unrelated parallel client attempts.
    const signature = attempt.errorSignature;
    const signatureCount = (this.internalRejectionSignatures.get(signature) || 0) + 1;
    this.internalRejectionSignatures.set(signature, signatureCount);
    dbg("internal client-tool rejection", "session=" + this.id, "kind=" + kind,
      "detail=" + detail, "total=" + this.invalidToolCalls, "category=" + categoryCount, "signature=" + signatureCount);
    if (signatureCount >= COMPOSER_MAX_IDENTICAL_INVALID_TOOL_CALLS || categoryCount >= COMPOSER_MAX_INVALID_TOOL_CALLS) {
      const reason = signatureCount >= COMPOSER_MAX_IDENTICAL_INVALID_TOOL_CALLS
        ? `client_tool_contract_mismatch: composer repeated the same internally rejected client-tool call (${kind}:${detail}); the tool was not executed`
        : `client_tool_contract_mismatch: composer run exceeded the ${kind} client-tool rejection bound (${COMPOSER_MAX_INVALID_TOOL_CALLS}); the tool was not executed`;
      this.tripLoopBound(reason, { error_code: "client_tool_contract_mismatch", executed: false });
      return true;
    }
    return false;
  }

  openClientTool({ source, rawToolCallId, name, input, resultAdapter }) {
    const settleAnnounced = source === "http-mcp"
      || (typeof source === "string" && source.startsWith("patched-"));
    let normalized;
    try {
      normalized = this.normalizeClientToolInput(name, input);
    } catch (error) {
      if (!(error instanceof ToolContractNormalizationError)) throw error;
      this.recordInternalToolRejection("normalization", `${name}:${error.code}`, {
        settleAnnounced,
        toolName: name,
        errors: [{ path: error.path || "arguments", keyword: error.code || "normalization" }],
        input,
      });
      const failure = this.internalContractFailure(name, error);
      return Promise.resolve(resultAdapter ? resultAdapter(failure) : failure);
    }
    input = normalized.value;
    if (normalized.transforms.length) {
      dbg("client tool contract translated SDK arguments", "session=" + this.id,
        "name=" + name, "transforms=" + safeJson(normalized.transforms));
    }
    const syntheticFailure = this.syntheticArtifactFailure(name, input);
    if (syntheticFailure) {
      dbg("synthetic agent-tools artifact blocked", "session=" + this.id, "source=" + source, "name=" + name);
      this.recordInternalToolRejection("synthetic-artifact", name, { settleAnnounced, toolName: name, input });
      return Promise.resolve(resultAdapter ? resultAdapter(syntheticFailure) : syntheticFailure);
    }
    const validationFailure = this.validateClientToolInput(name, input, normalized.transforms, { settleAnnounced });
    if (validationFailure) {
      return Promise.resolve(resultAdapter ? resultAdapter(validationFailure) : validationFailure);
    }
    const round = this.ensureToolRound();
    return new Promise((resolve, reject) => {
      let callbackSettled = false;
      const callback = {
        resolve: (result) => {
          if (callbackSettled) return;
          try {
            const adapted = resultAdapter ? resultAdapter(result) : result;
            callbackSettled = true;
            resolve(adapted);
          }
          catch (error) {
            callbackSettled = true;
            reject(error);
            throw error;
          }
        },
        reject: (error) => {
          if (callbackSettled) return;
          callbackSettled = true;
          reject(error);
        },
      };
      try {
        const call = round.openCall({
          source,
          rawToolCallId,
          name,
          input,
          callback,
          inventoryEpoch: this.toolInventoryEpoch,
          descriptorFingerprint: clientToolDescriptorFingerprint(this.toolRegistry.find(name)),
        });
        if (call.newlyRegistered) this.emitToolUse(call.wireId, name, input);
      } catch (error) {
        reject(error);
      }
    });
  }
  startPendingTimer(id) {
    const round = this.currentRound;
    if (!round) return;
    round.startTimer(id, PENDING_TIMEOUT_MS, () => {
      const detail = `tool ${id} abandoned after ${PENDING_TIMEOUT_MS}ms`;
      try { round.terminalize(TerminalReason.PENDING_TIMEOUT, detail); } catch {}
      this.lastRunError = `[bridge] ${detail}`;
      cancelSessionDetached(this, { terminalReason: TerminalReason.PENDING_TIMEOUT, detail });
    });
  }
  applyClientResults(results, round = this.currentRound) {
    if (!round) throw new ToolRoundError("round_lost", "no live ToolRound owns these results", 410);
    const applied = round.applyResults(results || []);
    return { ...applied, matched: applied.additions.length + applied.duplicates.length, unknown: [] };
  }

  dispatchUnary(cas, spec, s) {
    const ctx = { cwd: composerWorkspaceCwd(this.clientEnv) };
    const clientTool = this.reconcileToolName(spec.ccTool);
    if (!clientTool) {
      const detail = `no advertised client tool safely matches Cursor's native '${spec.ccTool}' capability`;
      dbg("native client tool unavailable", "session=" + this.id, "name=" + spec.ccTool);
      this.recordInternalToolRejection("native-unmatched", spec.ccTool, {
        settleAnnounced: true,
        input: ccArgsFor(cas, s),
      });
      return Promise.resolve({ __ccJson: spec.buildResult(detail, s, true, ctx) });
    }
    return this.openClientTool({
      source: "patched-unary",
      rawToolCallId: (s && s.toolCallId) || ccToolId(s),
      name: clientTool,
      input: ccArgsFor(cas, s),
      resultAdapter: (result) => ({ __ccJson: spec.buildResult(result.content, s, result.isError === true, ctx) }),
    });
  }
  dispatchStream(cas, spec, s) {
    const self = this;
    const ctx = { cwd: composerWorkspaceCwd(this.clientEnv) };
    const clientTool = this.reconcileToolName(spec.ccTool);
    return (async function* () {
      if (!clientTool) {
        const detail = `no advertised client tool safely matches Cursor's native '${spec.ccTool}' capability`;
        dbg("native streaming client tool unavailable", "session=" + self.id, "name=" + spec.ccTool);
        self.recordInternalToolRejection("native-unmatched", spec.ccTool, {
          settleAnnounced: true,
          input: ccArgsFor(cas, s),
        });
        for (const chunk of spec.buildChunks(detail, true, ctx)) yield { __ccJson: chunk };
        return;
      }
      const result = await self.openClientTool({
        source: "patched-stream",
        rawToolCallId: (s && s.toolCallId) || ccToolId(s),
        name: clientTool,
        input: ccArgsFor(cas, s),
        resultAdapter: (value) => value,
      });
      for (const chunk of spec.buildChunks(result.content, result.isError === true, ctx)) yield { __ccJson: chunk };
    })();
  }
  // The model called one of the client's advertised tools (incl MCPs). Reconcile the (often paraphrased)
  // name against the advertised set, then route to CC. CC's text result becomes the McpResult content.
  dispatchMcp(s) {
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
      this.recordInternalToolRejection("mcp-unmatched", want, { settleAnnounced: true, input });
      return Promise.resolve({ __ccJson: mcpDispatchResult(`Tool '${want}' is not available. Available tools: ${names || "(none)"}.`, true) });
    }
    return this.openClientTool({
      source: "patched-mcp",
      rawToolCallId: (s && s.toolCallId) || ccToolId(s),
      name: ccName,
      input,
      resultAdapter: (result) => {
        const norm = normalizeClientToolResult(
          result.content,
          result.isError,
          result.images,
          result.structuredContent,
          ccName,
          result.contentBlocks,
          result.structuredContentPresent,
        );
        this.rememberLargeMcpResult(norm.content, ccName);
        const imgs = mcpImageResultsEnabled() && norm.inlineImages ? norm.inlineImages : null;
        if (imgs) dbg("EX3 dispatchMcp folding image into McpToolResult (path A)", "session=" + this.id, "name=" + ccName, "images=" + imgs.length);
        return { __ccJson: mcpDispatchResult(
          norm.content,
          norm.isError,
          imgs,
          norm.structuredContent,
          norm.contentBlocks,
          norm.structuredContentPresent,
        ) };
      },
    });
  }
  // ADD-40: the effective advertised set the MODEL's tool calls are gated against. While a run is live this is
  // the TURN-SCOPED `activeAdvertise` (already restricted by tool_choice none/specific); outside a run it is
  // the full `advertise`. The model only dispatches tools DURING a run, so a tool_choice-disallowed tool can
  // never be reconciled/dispatched from the restored full set anymore.
  advertiseForGating() { return this.activeAdvertise != null ? this.activeAdvertise : (this.advertise || []); }
  reconcileToolName(want) {
    const adv = this.advertiseForGating();
    return resolveClientToolName(want, adv, {
      onRejectedSingle: (only) => dbg("reconcileToolName single-tool guard rejected implausible name",
        "session=" + this.id, "want=" + want, "only=" + only),
    });
  }

  emitToolUse(id, name, input, { handJournaled = false } = {}) {
    this.touch();
    const round = this.currentRound;
    const call = round && round.calls.get(id);
    if (!call) throw new ToolRoundError("unknown_tool_call_id", `cannot emit unregistered tool call ${id}`, 500);
    input = call.input;
    if (!this.activeRes || this.writeFailed || (round.state === RoundState.AWAITING_RESULTS && !handJournaled)) {
      // REGISTERED is itself the durable not-yet-handed queue. No second `undelivered` buffer exists.
      dbg("emitToolUse journaled for next response", "session=" + this.id, "id=" + id, "name=" + name);
      return;
    }
    dbg("emitToolUse", "session=" + this.id, "id=" + id, "name=" + name, "in=" + dbgInputShape(input));
    const receipt = this.sseReceipt({ type: "tool_call", id, name, input });
    if (!receipt.ok) return;
    receipt.handedToNode.then(() => {
      if (round.state === RoundState.TERMINAL) return;
      try { round.markHanded(id); }
      catch (error) {
        try { round.terminalize(TerminalReason.TRANSPORT_ERROR, error.message); } catch {}
        this.lastRunError = error.message;
        cancelSessionDetached(this, { terminalReason: TerminalReason.TRANSPORT_ERROR, detail: error.message });
        return;
      }
      this.startPendingTimer(id);
      const token = this.turnToken;
      if (this.flushTimer) clearTimeout(this.flushTimer);
      this.flushTimer = setTimeout(() => { if (token === this.turnToken) this.maybePauseForTools(token); }, TOOL_BATCH_MS);
    }).catch(() => {
      // The response-local writer owns transport terminalization and callback rejection.
    });
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
  // REGISTERED calls in the durable round are the only not-yet-handed queue. Re-emitting them uses the same
  // receipt path as a call produced while this response was already open.
  flushJournaledCalls() {
    const batch = this.queuedToolCalls();
    if (!batch.length || !this.activeRes || this.writeFailed) return false;
    dbg("flushJournaledCalls", "session=" + this.id, "count=" + batch.length, "ids=" + safeJson(batch.map((call) => call.wireId)));
    const round = this.currentRound;
    const refused = [];
    const executable = [];
    for (const call of batch) {
      const descriptor = this.toolRegistry.find(call.name);
      if (!descriptor) {
        refused.push({
          toolCallId: call.wireId,
          content: `Client tool '${call.name}' was removed before its queued call reached the harness. The tool was not executed.`,
          isError: true,
          structuredContent: {
            code: "client_tool_unavailable",
            executed: false,
            openedInventoryEpoch: call.inventoryEpoch,
            currentInventoryEpoch: this.toolInventoryEpoch,
            tool: call.name,
          },
        });
        continue;
      }
      const currentDescriptorFingerprint = clientToolDescriptorFingerprint(descriptor);
      if (call.descriptorFingerprint && call.descriptorFingerprint !== currentDescriptorFingerprint) {
        refused.push({
          toolCallId: call.wireId,
          content: `Client tool '${call.name}' changed its descriptor before its queued call reached the harness. The tool was not executed.`,
          isError: true,
          structuredContent: {
            code: "client_tool_schema_changed",
            executed: false,
            openedInventoryEpoch: call.inventoryEpoch,
            currentInventoryEpoch: this.toolInventoryEpoch,
            tool: call.name,
          },
        });
        continue;
      }
      const failure = this.toolRegistry.validate(call.name, call.input);
      if (failure) {
        refused.push({
          toolCallId: call.wireId,
          content: `Client tool '${call.name}' changed schema before its queued call reached the harness. The tool was not executed.`,
          isError: true,
          structuredContent: {
            code: "client_tool_schema_changed",
            errors: failure.structuredContent && failure.structuredContent.errors || [],
            executed: false,
            openedInventoryEpoch: call.inventoryEpoch,
            currentInventoryEpoch: this.toolInventoryEpoch,
            tool: call.name,
          },
        });
        continue;
      }
      executable.push(call);
    }
    let acted = false;
    if (refused.length) {
      const committed = round.commitResults(refused, { allowRegisteredReceipt: true });
      round.applyCommittedResults([...committed.additions, ...committed.duplicates]);
      this.lastSdkActivityAt = nowMs();
      acted = true;
    }
    if (round.state !== RoundState.TERMINAL) {
      for (const call of executable) {
        if (call.state !== CallState.REGISTERED) continue;
        this.emitToolUse(call.wireId, call.name, call.input, { handJournaled: true });
        acted = true;
      }
    }
    return acted;
  }
  // When the SDK has announced more tools for this step than the durable round has handed to the response,
  // wait one more debounce window for the rest of the
  // wave instead of pausing now — so a slow burst lands in ONE turn_end rather than spilling its tail into the
  // next turn's buffer. BOUNDED by TOOL_BATCH_MAX_EXTENSIONS so an over-count (an announced tool that never
  // dispatches) can never hang the turn. With no step signal (stepToolStarted==0, e.g. unit tests) it pauses
  // immediately — identical to the old debounce. It only ever DELAYS the pause, so it can never strand a tool.
  maybePauseForTools(token) {
    const batch = this.activeToolBatch();
    if (this.stepToolStarted > batch.length && this.batchWaitExtensions < TOOL_BATCH_MAX_EXTENSIONS) {
      this.batchWaitExtensions++;
      dbg("maybePauseForTools: awaiting full step batch", "session=" + this.id,
        "delivered=" + batch.length, "announced=" + this.stepToolStarted, "ext=" + this.batchWaitExtensions);
      this.flushTimer = setTimeout(() => { if (token === this.turnToken) this.maybePauseForTools(token); }, TOOL_BATCH_MS);
      return;
    }
    this.batchWaitExtensions = 0;
    this.pauseForTools();
  }
  pauseForTools() {
    this.flushTimer = null;
    const round = this.currentRound;
    const batch = this.activeToolBatch();
    if (!round || !batch.length || round.state === RoundState.TERMINAL) return;
    // ADD-106 (Comment 3): COUNT this tool-result round and enforce the per-logical-run loop bounds BEFORE
    // delivering the batch. If a bound trips, tear the run down as a MODEL/CLIENT-visible error terminal instead
    // of pausing for tools — so a runaway agentic loop (endless tool rounds, or the same tool+args hammered) can
    // never stream/pause forever, and is NEVER reported as a clean success.
    if (this.checkLoopBound()) return;
    round.markAwaitingResults();
    const receipt = this.sseReceipt({ type: "turn_end", stop_reason: "tool_use", tool_calls: batch.map((call) => call.wireId) });
    if (!receipt.ok) return;
    receipt.handedToNode.then(() => {
      round.batch = [];
      this.stepToolStarted = 0;
      this.batchWaitExtensions = 0;
      this.settle();
    }).catch(() => {});
  }
  settle() {
    const f = this.settleTurn;
    this.settleTurn = null;
    if (this.flushTimer) { clearTimeout(this.flushTimer); this.flushTimer = null; }
    this.lastSettledTurnToken = Math.max(this.lastSettledTurnToken, this.turnToken);
    const ready = this._turnDone.filter((waiter) => waiter.token <= this.lastSettledTurnToken);
    this._turnDone = this._turnDone.filter((waiter) => waiter.token > this.lastSettledTurnToken);
    for (const waiter of ready) { try { waiter.resolve(); } catch {} }
    if (f) f();
  }

  // resetLoopBounds clears the per-logical-run agentic-loop counters (ADD-106). Called when a FRESH send starts a
  // new logical run (driveUserSend) — NOT on a tool_results resume, which continues the SAME logical run and so
  // keeps accumulating rounds. A cancel/supersession also resets them via the next fresh send.
  resetLoopBounds() {
    this.toolRounds = 0;
    this.lastToolSig = null;
    this.repeatToolCount = 0;
    this.invalidToolCalls = 0;
    this.internalRejectionCounts.clear();
    this.internalRejectionSignatures.clear();
    this.loopTripped = false;
    this.stashedToolResultImages = [];
  }

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
    const batch = this.activeToolBatch();
    if (batch.length === 1) {
      const sig = batch[0].name + "\u0000" + safeJson(batch[0].input);
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
  tripLoopBound(reason, terminalDetails = null) {
    if (this.loopTripped) return;
    this.loopTripped = true;
    this.lastRunError = reason; // BR2: a later tool_results that finds the run gone surfaces this real error
    dbg("tripLoopBound", "session=" + this.id, "rounds=" + this.toolRounds,
      "repeat=" + this.repeatToolCount, "invalid=" + this.invalidToolCalls, reason);
    this.rejectAllPending(reason, TerminalReason.LOOP_BOUND);
    this.sse({ type: "turn_end", stop_reason: "error", error: reason, ...(terminalDetails || {}) });
    this.settle();
    // cancel() tears down the live run/agent, rejects every pending, bumps runEpoch (epoch-gating the dead run's
    // callbacks), and fires notifyLogicalDone so a queued new-user turn is admitted. done=true short-circuits any
    // late onRunComplete/onRunError from the cancelled run so the error terminal above is the run's only terminal.
    cancelSessionDetached(this, { terminalReason: TerminalReason.LOOP_BOUND, detail: reason });
  }

  onRunComplete(res) {
    if (this.done) return;
    const completedClientMessageId = this.activeClientMessageId;
    const completedClientMessageHash = this.activeClientMessageHash;
    const completedClientMessageGeneration = this.activeClientMessageGeneration;
    const completedClientMessageKind = this.activeClientMessageKind;
    const completedIdentityPolicy = this.activeIdentityPolicy;
    const completedDeferredInputId = this.activeDeferredInputId;
    const completedDeferredRound = completedDeferredInputId
      ? (this.recoverySourceRound
        || (this.currentRound && this.currentRound.deferredInput(completedDeferredInputId)
          ? this.currentRound : null))
      : null;
    this.done = true; this.run = null; this.sendPending = false;
    const runError = res && res.error != null ? res.error : null;
    // BR2: a non-"finished" terminal means the upstream run failed; remember it so a tool_results turn that
    // finds nothing to resume surfaces the real error instead of a false-success empty turn.
    if (res && res.status !== "finished") this.lastRunError = runError || "run did not finish";
    dbg("onRunComplete", "session=" + this.id, "status=" + (res && res.status),
      "error=" + (runError == null ? "(none)" : safeJson(runError)),
      "streamedTextLen=" + this.streamedText.length, "resultLen=" + ((res && res.result) || "").length);
    // text-delta deltas already streamed the full text incrementally. Only fall back to the
    // res.result lump if NO deltas fired this run (non-streaming edge) — otherwise we'd duplicate.
    const fullResult = (res && res.result) || "";
    if (!this.streamedText && fullResult) this.sse({ type: "text", delta: fullResult });
    // A run that "finished" but produced no user-visible output this run — no streamed
    // text, no result lump, and no tool call delivered OR buffered — is an EMPTY turn, not a success. Surface it
    // as turn_end{error} so the client is never handed a clean empty completion. Durable ToolRound state
    // accumulates across the run's tool rounds (clearTurnState runs only at the terminal), so
    // this is accurate for a multi-round run and only fires on a genuinely empty finished run.
    const finished = res && res.status === "finished";
    // #15: reasoning, still-buffered catch-up deltas, and any reported usage ALSO count as produced output, so a
    // run that emitted only reasoning (or whose text is buffered awaiting flush) is not mis-flagged as an empty
    // turn (a false error). The empty->error path fires ONLY on a finished run with genuinely nothing produced.
    const hasUsage = !!(res && res.usage && typeof res.usage === "object" && Object.keys(res.usage).length > 0);
    const producedOutput = !!this.streamedText || this.reasonedThisRun || !!fullResult
      || this.toolRounds > 0 || this.queuedToolCalls().length > 0 || this.pendingDeltas.length > 0 || hasUsage;
    const unresolvedTools = this.pendingCount() > 0 || this.queuedToolCalls().length > 0;
    let stopReason = finished && producedOutput && !unresolvedTools ? "end_turn" : "error";
    let turnError = unresolvedTools
      ? "composer run completed while client tools remained unresolved"
      : finished
      ? (producedOutput ? runError : "composer run finished with no output (empty turn)")
      : (runError || "composer run did not finish");
    if (stopReason === "end_turn" && this.recoverySourceRound) {
      try {
        const prior = this.recoverySourceRound.recovery || {};
        this.recoverySourceRound.recordRecovery({
          ...prior,
          completedAt: nowMs(),
          decision: "completed",
          replacementAgentId: this.agentId,
        });
        this.recoverySourceRound = null;
      } catch (error) {
        // A clean final response is not safe until the recovery completion
        // receipt is durable. Surface an error so an identical harness retry
        // can recover again instead of silently starting duplicate work.
        stopReason = "error";
        turnError = `failed to persist recovery completion: ${(error && error.message) || error}`;
      }
    }
    if (stopReason !== "end_turn" && this.recoverySourceRound) {
      try {
        const prior = this.recoverySourceRound.recovery || {};
        this.recoverySourceRound.recordRecovery({
          ...prior,
          decision: "failed",
          failedAt: nowMs(),
          reason: turnError || "replacement run did not complete cleanly",
        });
      } catch {}
      this.recoverySourceRound = null;
      if (sessions.get(this.id) === this) sessions.delete(this.id);
    }
    this.rejectAllPending("run completed while client tools remained unresolved", TerminalReason.RUN_ERROR);
    let terminal = { type: "turn_end", stop_reason: stopReason, status: res && res.status, usage: (res && res.usage) || {} };
    if (turnError != null && turnError !== "") terminal.error = turnError;
    let completedReceiptPersisted = false;
    if (stopReason === "end_turn" && completedClientMessageId) {
      try {
        const replayEvents = this.completedReplayEvents(terminal);
        writeCompletedTurnReceipt(
          this.cursorKey,
          this.id,
          completedClientMessageId,
          completedClientMessageHash,
          replayEvents,
          terminal.usage,
          {
            generation: completedClientMessageGeneration,
            replace: completedClientMessageKind === "fresh",
            requestKind: completedClientMessageKind,
            clientLeaseToken: this.clientLeaseToken,
            identityPolicy: completedIdentityPolicy,
          },
        );
        completedReceiptPersisted = true;
        dbg("completed-turn receipt committed", "session=" + this.id,
          "identity=" + completedClientMessageId, "hash=" + completedClientMessageHash);
      } catch (error) {
        // The model may have completed, but without a durable ordered replay
        // log the bridge cannot prove or reproduce that completion after a
        // restart. Never acknowledge it as success: retain the UNKNOWN/RUNNING
        // fresh attempt and surface a typed delivery-unknown terminal so the
        // exact SDK idempotency key remains the only legal retry.
        const detail = (error && error.message) || String(error);
        stopReason = "error";
        terminal = {
          type: "turn_end",
          stop_reason: "error",
          receipt: "completion_not_durable",
          retryable: true,
          retryMode: "identical",
          deliveryUnknown: true,
          error: `the model completed but its durable replay receipt could not be committed: ${detail}`,
        };
        dbg("completed-turn receipt persistence failed; returning delivery-unknown",
          "session=" + this.id, "clientMessageId=" + completedClientMessageId,
          detail);
      }
    }
    // agent.send resolved before onRunComplete can run, so even a non-finished
    // terminal is acceptance-unknown rather than a proven pre-accept failure.
    // Retain UNKNOWN/RUNNING and the frozen SDK envelope; authorizing a new
    // generation here could execute the same invocation twice.
    // A successful run with its replay receipt committed proves the upstream
    // connection is healthy. Legacy no-id turns retain their historical
    // non-replayable behavior; every versioned turn must cross the commit.
    if (stopReason === "end_turn" && (!completedClientMessageId || completedReceiptPersisted)) {
      closeBreaker(keyHash(this.cursorKey));
    }
    if (completedReceiptPersisted && completedDeferredRound && completedDeferredInputId) {
      try {
        completedDeferredRound.markDeferredInputState(
          completedDeferredInputId,
          DeferredInputState.DELIVERED,
          { finalized: true, evidence: "completed_turn_receipt" },
        );
      } catch (error) {
        // The final response receipt is authoritative. Failure to compact the
        // larger delivery envelope is a storage-retention issue, not a reason
        // to hide a completed model answer.
        dbg("deferred delivery finalization cleanup failed", "session=" + this.id,
          (error && error.message) || String(error));
      }
    }
    this.sse(terminal);
    this.activeClientMessageId = ""; this.activeClientMessageHash = "";
    this.activeClientMessageGeneration = 1; this.activeClientMessageKind = "";
    this.activeIdentityPolicy = TURN_IDENTITY_POLICY.NONE; this.activeDeferredInputId = "";
    this.clearTurnState();
    this.settle();
    this.notifyLogicalDone(); // real completion -> admit the next queued new-user turn
  }
  onRunError(err) {
    if (this.done) return;
    const failedClientMessageId = this.activeClientMessageId;
    const failedClientMessageHash = this.activeClientMessageHash;
    const failedClientMessageKind = this.activeClientMessageKind;
    this.done = true; this.run = null; this.sendPending = false;
    const msg = (err && err.message) || String(err);
    // run.wait() exists only after agent.send resolved. A rejection therefore
    // cannot prove that the remote rejected the invocation before acceptance.
    // Keep the durable attempt unresolved and retry only its frozen envelope.
    this.activeClientMessageId = ""; this.activeClientMessageHash = "";
    this.activeClientMessageGeneration = 1; this.activeClientMessageKind = "";
    this.activeIdentityPolicy = TURN_IDENTITY_POLICY.NONE; this.activeDeferredInputId = "";
    this.lastRunError = msg; // BR2: a tool_results turn that finds the run gone surfaces this real error
    if (this.recoverySourceRound) {
      try {
        const prior = this.recoverySourceRound.recovery || {};
        this.recoverySourceRound.recordRecovery({ ...prior, decision: "failed", failedAt: nowMs(), reason: msg });
      } catch {}
      this.recoverySourceRound = null;
      // A failed recovery is retryable from the durable source round. Remove
      // the replacement Session so the retry cannot collide with a dead run.
      if (sessions.get(this.id) === this) sessions.delete(this.id);
    }
    dbg("onRunError", "session=" + this.id, (err && err.stack) || msg);
    this.rejectAllPending("run errored", TerminalReason.RUN_ERROR);
    this.sse({ type: "turn_end", stop_reason: "error", error: msg });
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
    if (isConversationTooLong(msg)) {
      void this.rotateDurableAgent().catch((error) => {
        dbg("rotateDurableAgent failed", "session=" + this.id, (error && error.message) || String(error));
        sessions.delete(this.id);
        void this.cancel({
          terminalReason: TerminalReason.RUN_ERROR,
          detail: "durable agent rotation failed",
        }).catch((cancelError) => {
          dbg("failed rotation cleanup also failed", "session=" + this.id,
            (cancelError && cancelError.message) || String(cancelError));
        });
      });
    }
  }
  // A floating SDK AbortError is not the normal run.wait() rejection and does
  // not carry the Session. Once the process-level handler has safely
  // attributed it, discard the SDK agent as well as the run: reusing an agent
  // whose transport just aborted can turn one interrupted turn into a
  // permanent active-run wedge.
  abortFromSdk(err) {
    if (this.done) return;
    const agentToClose = this.agent;
    this.onRunError(err);
    this.agent = null;
    this.agentPromise = null;
    this.mcpServerKeys = null;
    if (agentToClose && typeof agentToClose.close === "function") {
      Promise.resolve()
        .then(() => agentToClose.close())
        .catch((closeError) => dbg("SDK abort agent cleanup failed", "session=" + this.id,
          (closeError && closeError.message) || String(closeError)));
    }
  }
  // composeAgentId derives the durable agentId from the external id plus BOTH rotation epochs (C05 too-long
  // rotation + ADD-62 model-change rotation). Epoch 0/0 keeps the id == external id (the original behavior, so
  // existing durable state is unchanged); any rotation appends a stable, unique suffix (`_rN` and/or `_mN`).
  // Combining both epochs guarantees a fresh unique id even when a model change follows a too-long rotation.
  composeAgentId({
    recoveryEpoch = this.recoveryEpoch,
    modelEpoch = this.modelEpoch,
    keyEpoch = this.keyEpoch,
    contextEpoch = this.contextEpoch,
  } = {}) {
    let aid = `${this.id}_${DURABLE_AGENT_CONTEXT_EPOCH}`;
    if (recoveryEpoch > 0) aid += `_r${recoveryEpoch + 1}`; // first too-long rotation -> _r2
    if (modelEpoch > 0) aid += `_m${modelEpoch}`;           // first model change   -> _m1
    if (keyEpoch > 0) aid += `_k${keyEpoch}`;               // ADD-79: first key change -> _k1
    if (contextEpoch > 0) aid += `_c${contextEpoch}`;       // first system-context replacement -> _c1
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
    const oldAgentId = this.agentId;
    const nextRecoveryEpoch = this.recoveryEpoch + 1;
    const targetAgentId = this.composeAgentId({ recoveryEpoch: nextRecoveryEpoch });
    // The durable alias is the rotation commit point. If fsync/rename fails,
    // no in-memory identity changes and the next request can retry safely.
    writeDurableAgentAlias(this.cursorKey, this.id, targetAgentId, {
      recoveryEpoch: nextRecoveryEpoch,
      modelEpoch: this.modelEpoch,
      keyEpoch: this.keyEpoch,
      contextEpoch: this.contextEpoch,
      systemBlockIds: [],
    });
    this.recoveryEpoch = nextRecoveryEpoch;
    this.agentId = targetAgentId;
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
  // the new model. The external session id is KEPT so continuations keep routing here. Per the durability contract:
  // rotate + re-seed (NOT 409-reject, NOT silently reuse the old model's agent). Bounded so pathological
  // model-flapping cannot churn agentIds without limit; past the cap we keep the last rotated agent (the next
  // turn re-seeds into it) rather than dropping the session.
  async rotateForModelChange(newModel) {
    const COMPOSER_MAX_MODEL_ROTATIONS = 8;
    const oldAgentId = this.agentId;
    const oldModel = this.model;
    const nextModelEpoch = this.modelEpoch < COMPOSER_MAX_MODEL_ROTATIONS
      ? this.modelEpoch + 1 : this.modelEpoch;
    const targetAgentId = this.composeAgentId({ modelEpoch: nextModelEpoch });
    if (nextModelEpoch === this.modelEpoch) {
      dbg("rotateForModelChange cap reached -> keep last agentId, force re-seed", "session=" + this.id, "agentId=" + this.agentId, "epoch=" + this.modelEpoch);
    }
    writeDurableAgentAlias(this.cursorKey, this.id, targetAgentId, {
      recoveryEpoch: this.recoveryEpoch,
      modelEpoch: nextModelEpoch,
      keyEpoch: this.keyEpoch,
      contextEpoch: this.contextEpoch,
      systemBlockIds: [],
    });
    this.modelEpoch = nextModelEpoch;
    this.agentId = targetAgentId;
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
    const nextKeyEpoch = this.keyEpoch < COMPOSER_MAX_KEY_ROTATIONS
      ? this.keyEpoch + 1 : this.keyEpoch;
    const targetAgentId = this.composeAgentId({ keyEpoch: nextKeyEpoch });
    const targetKey = newKey || API_KEY;
    if (nextKeyEpoch === this.keyEpoch) {
      dbg("rotateForKeyChange cap reached -> keep last agentId, force re-seed", "session=" + this.id, "agentId=" + this.agentId, "epoch=" + this.keyEpoch);
    }
    writeDurableAgentAlias(targetKey, this.id, targetAgentId, {
      recoveryEpoch: this.recoveryEpoch,
      modelEpoch: this.modelEpoch,
      keyEpoch: nextKeyEpoch,
      contextEpoch: this.contextEpoch,
      systemBlockIds: [],
    });
    this.keyEpoch = nextKeyEpoch;
    this.agentId = targetAgentId;
    this.cursorKey = targetKey; // run subsequent turns on the NEW key's platform/account
    this.resetSeedState();
    dbg("rotateForKeyChange -> rotate durable agentId for new key (no resumeAgent(old key's agent))", "session=" + this.id, "old=" + oldAgentId, "new=" + this.agentId);
    await this.finishRotationCancel();
  }
  // A true system-context replacement/removal/reorder cannot be represented by
  // appending another instruction to an already-contextualized model. Rotate to
  // a fresh durable agent and faithfully re-seed bounded history instead. This
  // is role/provenance based and independent of any client or harness name.
  async rotateForSystemReplacement(nextBlockIds) {
    if (!validSystemBlockIds(nextBlockIds)) {
      throw new ToolRoundError("invalid_system_blocks", "replacement system block identity is invalid", 422);
    }
    if (this.contextEpoch >= 1024) {
      throw new ToolRoundError(
        "system_context_rotation_exhausted",
        "system context changed too many times for this durable conversation",
        503,
      );
    }
    const oldAgentId = this.agentId;
    const nextContextEpoch = this.contextEpoch + 1;
    const targetAgentId = this.composeAgentId({ contextEpoch: nextContextEpoch });
    writeDurableAgentAlias(this.cursorKey, this.id, targetAgentId, {
      recoveryEpoch: this.recoveryEpoch,
      modelEpoch: this.modelEpoch,
      keyEpoch: this.keyEpoch,
      contextEpoch: nextContextEpoch,
      systemBlockIds: nextBlockIds,
    });
    this.contextEpoch = nextContextEpoch;
    this.agentId = targetAgentId;
    this.resetSeedState();
    this.seededSystemBlockIds = [...nextBlockIds];
    dbg("rotateForSystemReplacement -> rotate and faithfully re-seed",
      "session=" + this.id, "old=" + oldAgentId, "new=" + targetAgentId);
    await this.finishRotationCancel();
  }
  // A rewritten/compacted transcript is a context replacement just like a
  // changed system block set: it cannot be appended to the old durable agent.
  // Commit a fresh context epoch before closing the old handle, then re-seed
  // the bounded replacement history into that new agent.
  async rotateForHistoryReplacement() {
    if (this.contextEpoch >= 1024) {
      throw new ToolRoundError(
        "history_context_rotation_exhausted",
        "conversation history changed too many times for this durable conversation",
        503,
      );
    }
    const oldAgentId = this.agentId;
    const nextContextEpoch = this.contextEpoch + 1;
    const targetAgentId = this.composeAgentId({ contextEpoch: nextContextEpoch });
    const systemBlockIds = validSystemBlockIds(this.seededSystemBlockIds)
      ? [...this.seededSystemBlockIds] : [];
    writeDurableAgentAlias(this.cursorKey, this.id, targetAgentId, {
      recoveryEpoch: this.recoveryEpoch,
      modelEpoch: this.modelEpoch,
      keyEpoch: this.keyEpoch,
      contextEpoch: nextContextEpoch,
      systemBlockIds,
    });
    this.contextEpoch = nextContextEpoch;
    this.agentId = targetAgentId;
    this.resetSeedState();
    this.seededSystemBlockIds = systemBlockIds;
    dbg("rotateForHistoryReplacement -> rotate and faithfully re-seed",
      "session=" + this.id, "old=" + oldAgentId, "new=" + targetAgentId);
    await this.finishRotationCancel();
  }
  // An all-legacy `call_*` continuation has no signed ToolRound to resume after the bridge upgrade. Move it
  // to one deterministic replacement agent, persist the alias before the first SDK send, and seed that agent
  // from the faithful client replay. Retrying the same request selects the same agent and SDK idempotency key.
  async rotateForLegacyRecovery(recoveryKey) {
    if (!/^clr1_[0-9a-f]{24}$/.test(String(recoveryKey || ""))) {
      throw new Error("legacy recovery key is malformed");
    }
    const target = `${this.id}_legacy_${recoveryKey.slice("clr1_".length)}`;
    if (this.agentId === target) {
      writeDurableAgentAlias(this.cursorKey, this.id, target, this);
      return;
    }
    const oldAgentId = this.agentId;
    writeDurableAgentAlias(this.cursorKey, this.id, target, {
      recoveryEpoch: this.recoveryEpoch,
      modelEpoch: this.modelEpoch,
      keyEpoch: this.keyEpoch,
      contextEpoch: this.contextEpoch,
      systemBlockIds: [],
    });
    this.agentId = target;
    this.model = null;
    this.resetSeedState();
    dbg("rotateForLegacyRecovery -> deterministic faithful replay agent", "session=" + this.id, "old=" + oldAgentId, "new=" + target);
    await this.finishRotationCancel();
    this.currentRound = null;
    this.recoverySourceRound = null;
  }
  rejectAllPending(why, reason = TerminalReason.RUN_ERROR) {
    if (this.currentRound && this.currentRound.state !== RoundState.TERMINAL) {
      this.currentRound.terminalize(reason, why);
    }
  }
  clearTurnState() {
    if (this.flushTimer) { clearTimeout(this.flushTimer); this.flushTimer = null; }
    this.stepToolStarted = 0; this.batchWaitExtensions = 0; // #85: reset the step batch counters at the terminal
    this.pendingDeltas = []; this.pendingDeltaBytes = 0; // #14: drop any undelivered text/reasoning; the run is over
    this.beginReplaySegment(); // completion has committed it, or the failed/cancelled turn must release it
    this.activeAdvertise = null; // ADD-40: the turn-scoped effective tool policy ends with the run
  }
  emitCancellationTerminalReceipt(terminalReason, detail) {
    if (!this.activeRes || !this.responseWriter || this.writeFailed
        || this.activeRes.writableEnded || this.activeRes.destroyed
        || this.lastTerminalTurnToken >= this.turnToken) {
      return null;
    }
    const retryable = terminalReason === TerminalReason.SHUTDOWN;
    return this.sseReceipt({
      type: "turn_end",
      stop_reason: "error",
      receipt: "session_cancellation_terminal",
      cancellationReason: terminalReason,
      retryable,
      ...(retryable ? { retryMode: "identical" } : {}),
      error: String(detail || "session cancelled").replace(/\s+/g, " ").slice(0, 1024),
    });
  }
  emitCancellationTerminal(terminalReason, detail) {
    return !!this.emitCancellationTerminalReceipt(terminalReason, detail)?.ok;
  }
  // cancel tears down the live run/agent + rejects pendings. ADD-90: `notify` controls whether queued waiters
  // are released (notifyLogicalDone). External callers (onClose, handleTurn interrupt, rotate*, shutdown, evict,
  // failWrite) use the default notify:true so the FIFO advances. Internal SUPERSESSION (cancelStaleRun) passes
  // notify:false so a queued new-user turn is NOT promoted before driveUserSend installs the replacement
  // session.run — otherwise the queued turn and the replacement send would race on the same durable agent.
  async cancel({ notify = true, terminalReason = TerminalReason.CLIENT_CANCELLED, detail = "session cancelled" } = {}) {
    if (notify) this.cancelNotifyRequested = true;
    if (this.cancelPromise) {
      await this.cancelPromise;
      return;
    }
    this.cancelNotifyRequested = notify;
    const cancelPromise = this.cancelOnce({ terminalReason, detail });
    this.cancelPromise = cancelPromise;
    try {
      await cancelPromise;
    } finally {
      if (this.cancelPromise === cancelPromise) this.cancelPromise = null;
      this.cancelNotifyRequested = false;
    }
  }
  async cancelOnce({ terminalReason = TerminalReason.CLIENT_CANCELLED, detail = "session cancelled" } = {}) {
    const wasDone = this.done;
    const runToCancel = this.run;
    const agentToClose = this.agent;
    const cancelledRunEpoch = this.runEpoch;
    // The SDK's AbortError has no Session/request identity. Record the local
    // cancellation cause before invoking the SDK so a later floating abort
    // can be correlated with a disconnect, supersession, transport failure,
    // eviction, or shutdown in production logs.
    dbg("session.cancel",
      "session=" + this.id,
      "reason=" + terminalReason,
      "detail=" + String(detail || "").replace(/\s+/g, " ").slice(0, 256),
      "wasDone=" + wasDone,
      "run=" + !!runToCancel,
      "activeRes=" + !!this.activeRes,
      "round=" + (this.currentRound ? this.currentRound.state : "none"),
      "pending=" + this.pendingCount(),
      "waiters=" + this.waiters,
      "notify=" + this.cancelNotifyRequested);
    // A live HTTP response must always receive a terminal receipt before its
    // run state is cleared. Previously an interrupt/shutdown called settle()
    // and finishResponse() with only [DONE], which Go correctly classified as
    // a protocol EOF and surfaced as a client-visible 502. Queueing the typed
    // terminal through the existing SseWriter preserves ordering/backpressure;
    // the runTurn finally block appends [DONE] after it.
    if (!wasDone) this.emitCancellationTerminal(terminalReason, detail);
    this.done = true;     // short-circuit any late run.wait() settlement (onRunComplete/onRunError no-op on done)
    this.runEpoch++;      // invalidate the in-flight run's completion callback so it can't mutate a successor turn
    this.rejectAllPending(detail, terminalReason);
    this.clearTurnState();
    if ((runToCancel && typeof runToCancel.cancel === "function")
        || (agentToClose && typeof agentToClose.close === "function")) {
      noteExpectedSdkAbort(this.id, cancelledRunEpoch);
      // AsyncLocalStorage follows promises/timers created by SDK cleanup. When
      // one of those detached promises later reaches unhandledRejection, this
      // is the only exact session/run-generation correlation the SDK exposes.
      const cleanup = als.run({
        session: this,
        sdkCancellation: { sessionId: this.id, runEpoch: cancelledRunEpoch },
      }, async () => {
        try { await (runToCancel && runToCancel.cancel && runToCancel.cancel()); } catch {}
        try { await (agentToClose && agentToClose.close && agentToClose.close()); } catch {}
      });
      let cleanupTimer = null;
      const abandoned = new Promise((_, reject) => {
        cleanupTimer = setTimeout(() => {
          const error = new Error(`Cursor SDK cancellation cleanup exceeded ${CANCEL_CLEANUP_MS}ms`);
          try {
            console.error("[cursor-agent-bridge] SDK cancellation cleanup abandoned; restarting isolated sidecar session="
              + this.id + " runEpoch=" + cancelledRunEpoch + ":", error.message);
          } catch {}
          // This guard starts only after cancellation was explicitly requested;
          // it never times out an established upstream data path. Restarting is
          // safer than reusing an agent whose old run may still be alive.
          void shutdown(1, false);
          reject(error);
        }, CANCEL_CLEANUP_MS);
      });
      try {
        await Promise.race([cleanup, abandoned]);
      } finally {
        if (cleanupTimer) clearTimeout(cleanupTimer);
      }
    }
    this.run = null;
    this.sendPending = false;
    this.activeClientMessageId = "";
    this.activeClientMessageHash = "";
    this.activeClientMessageGeneration = 1;
    this.activeClientMessageKind = "";
    this.activeIdentityPolicy = TURN_IDENTITY_POLICY.NONE;
    this.activeDeferredInputId = "";
    // Null the closed agent handle so a surviving queued waiter (the session is kept when waiters remain)
    // re-resumes/recreates a live agent via ensureAgent instead of reusing this dead one.
    this.agent = null; this.agentPromise = null;
    this.mcpServerKeys = null;
    this.settle();
    if (this.cancelNotifyRequested) this.notifyLogicalDone(); // run torn down -> release any queued waiter so the chain advances
  }
}

function nowMs() { return Date.now(); }

// Every fire-and-forget cancellation crosses this boundary. Session.cancel is
// single-flight, and this catch prevents future journal/cleanup changes from
// leaking a detached rejection into the process-wide fatal handler.
function cancelSessionDetached(session, options) {
  Promise.resolve()
    .then(() => session.cancel(options))
    .catch((error) => {
      try {
        console.error("[cursor-agent-bridge] session-local cancellation cleanup failed; failure contained session="
          + (session && session.id || "?") + ":", (error && error.stack) || error);
      } catch {}
    });
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
// fork's GPT-style dash-suffix convention (e.g. gpt-5.2-xhigh). Confirmed via Cursor.models.list() +
// `cursor-agent models` on @cursor/sdk@1.0.23 / local CLI:
//
//   composer-2.5 / composer-2:
//     DEFAULT variant is fast=true (costly). Mapping:
//       composer-2.5         -> { fast:false }
//       composer-2.5-fast    -> { fast:true }
//       composer-2.5-<level> -> { fast:false, thinking:<level> }  (SDK param id is `thinking`)
//
//   Cursor Grok 4.5 (SDK id "grok-4.5"; display "Cursor Grok 4.5"):
//     CLIENT-FACING ids are namespaced `cursor-grok-4.5*` so they never collide with xAI's `grok-4.5`
//     (owned_by xai) on /v1/models. The bridge strips the `cursor-` prefix, then maps onto the SDK id.
//     Params: effort ∈ {low,medium,high}, fast ∈ {false,true}. DEFAULT variant is effort=high + fast=true
//     (costly). CLI lists grok-4.5-xhigh / grok-4.5-fast-xhigh / …. Mapping (xhigh/max → high; minimal/none → low):
//       cursor-grok-4.5              -> { id:grok-4.5, fast:false, effort:high }
//       cursor-grok-4.5-fast         -> { id:grok-4.5, fast:true,  effort:high }
//       cursor-grok-4.5-low          -> { id:grok-4.5, fast:false, effort:low }
//       cursor-grok-4.5-fast-medium  -> { id:grok-4.5, fast:true,  effort:medium }
//       cursor-grok-4.5-xhigh        -> { id:grok-4.5, fast:false, effort:high }
//     Bare SDK/CLI forms (grok-4.5, grok-4.5-fast, …) are still accepted when the request is already on the
//     cursor bridge path (e.g. a model alias that rewrites to the upstream SDK id).
//
// Non-recognized ids pass through unchanged (Cursor resolves their own default). Composer thinking levels are
// passed THROUGH (Cursor validates). Grok effort is clamped to the SDK's {low,medium,high} set.
const COMPOSER_THINKING_LEVELS = new Set(["minimal", "none", "low", "medium", "high", "xhigh", "max"]);
// Map GPT/CLI-style effort suffixes onto the grok-4.5 SDK `effort` values (only low|medium|high exist).
function mapGrokEffort(level) {
  if (!level) return null;
  const l = String(level).toLowerCase();
  if (l === "low" || l === "medium" || l === "high") return l;
  if (l === "xhigh" || l === "max") return "high";
  if (l === "minimal" || l === "none") return "low";
  return null;
}
function composerModelSelection(model, { utilityOneShot = false } = {}) {
  const raw = String(model || "");
  let id = raw;
  // Disambiguate from xAI grok-4.5: client-facing Cursor ids are cursor-grok-4.5*. Strip only that prefix.
  if (/^cursor-grok-4\.5/i.test(id)) id = id.slice("cursor-".length);
  let fast = "false";
  let thinking = null;
  // Suffix order is base[-fast][-<level>]: strip the innermost reasoning level first, then the -fast variant, so
  // composer-2.5-fast-high -> { fast:true, thinking:high }, composer-2.5-high -> { fast:false, thinking:high }.
  // Same strip applies to (cursor-)grok-4.5-fast-xhigh -> base grok-4.5 + fast + effort.
  const d = id.lastIndexOf("-");
  if (d > 0 && COMPOSER_THINKING_LEVELS.has(id.slice(d + 1).toLowerCase())) {
    thinking = id.slice(d + 1).toLowerCase();
    id = id.slice(0, d);
  }
  if (/-fast$/.test(id)) { fast = "true"; id = id.slice(0, id.length - "-fast".length); }
  if (id === "composer-2.5" || id === "composer-2") {
    if (utilityOneShot) return { id, params: [{ id: "fast", value: "false" }, { id: "thinking", value: "low" }] };
    const params = [{ id: "fast", value: fast }];
    if (thinking) params.push({ id: "thinking", value: thinking });
    return { id, params };
  }
  if (id === "grok-4.5") {
    if (utilityOneShot) return { id: "grok-4.5", params: [{ id: "fast", value: "false" }, { id: "effort", value: "low" }] };
    // Bare / missing level => effort=high (matches CLI's primary "Cursor Grok 4.5" = grok-4.5-xhigh).
    // Always set fast explicitly so we never silently inherit Cursor's costly fast=true default.
    // SDK id stays "grok-4.5" regardless of the client-facing cursor- prefix.
    const effort = mapGrokEffort(thinking) || "high";
    return { id: "grok-4.5", params: [{ id: "fast", value: fast }, { id: "effort", value: effort }] };
  }
  return { id: raw }; // non-recognized: pass the original id through (Cursor resolves its default)
}

async function ensureAgent(session, model) {
  if (session.agent) return session.agent;
  if (session.agentPromise) return session.agentPromise;          // guard TOCTOU
  session.agentPromise = (async () => {
    const platform = await getPlatform(session.cursorKey);
    const modelSel = composerModelSelection(model, { utilityOneShot: session.utilityOneShot });
    dbg("ensureAgent modelSelection", "session=" + session.id, "model=" + model, "selection=" + safeJson(modelSel));
    const opts = { model: modelSel, apiKey: session.cursorKey, local: { cwd: EMPTY_CWD } };
    // MCP shim registration (additive, never substitutive): attach the session's MCP server map so the SDK's
    // local runtime dials our in-bridge /mcp/<id> endpoint and surfaces the advertised tools to the model.
    // Wrapped so any throw degrades to today's native-only behavior — the working read/shell path MUST survive
    // any shim failure. Built per ensureAgent so a session whose tools change across runs re-registers correctly,
    // and applied to BOTH the resume and create branches below (they spread the same opts).
    try {
      const servers = buildMcpServers(session);
      if (servers && Object.keys(servers).length) {
        opts.mcpServers = servers;
        session.mcpServerKeys = Object.freeze(Object.keys(servers));
      }
    } catch (e) {
      session.mcpServerKeys = null;
      dbg("ensureAgent buildMcpServers failed (continuing native-only)", "session=" + session.id, (e && e.message) || String(e));
    }
    // C05: resume/create against the DURABLE agentId (rotates on too-long), not the external session id.
    const agentId = session.agentId || session.id;
    try {
      if (session.utilityOneShot) {
        dbg("ensureAgent createAgent (ephemeral utility)", "session=" + session.id, "agentId=" + agentId);
        session.agent = await platform.createAgent({ agentId, ...opts });
        return session.agent;
      }
      dbg("ensureAgent resumeAgent", "session=" + session.id, "agentId=" + agentId, "model=" + model, "mcpServers=" + (opts.mcpServers ? Object.keys(opts.mcpServers).length : 0));
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
// same ToolRound adapter as patched MCP dispatch — the model's call becomes an SSE tool_call the client
// answers on a later /agent/continue request.

// mcpServerKeyForTool maps an advertised tool NAME to its server key under the active grouping. Pure helper.
//   one      -> always the generic client-tools key.
//   natural  -> mcp__<server>__<tool> -> sanitize(<server>); everything else -> client-tools.
//   per-tool -> sanitize(<toolName>) (one server per tool).
// sanitize restricts a key to a URL-safe segment [A-Za-z0-9_.-] (other chars ->
// "-") and excludes exact dot segments, which URL clients may normalize away.
function mcpSanitizeKey(s) {
  const key = String(s || "").replace(/[^A-Za-z0-9_.-]/g, "-");
  if (key === ".") return "server-dot";
  if (key === "..") return "server-dotdot";
  return key || DEFAULT_MCP_SERVER_KEY;
}
function mcpServerKeyForTool(name, grouping = MCP_GROUPING) {
  const n = String(name || "");
  if (grouping === "one") return DEFAULT_MCP_SERVER_KEY;
  if (grouping === "per-tool") return mcpSanitizeKey(n);
  // natural: reconstruct the originating MCP server from the mcp__<server>__<tool> convention. Non-greedy
  // first group so the FIRST "__" after the prefix delimits server vs tool (a server token may itself carry
  // single underscores, e.g. plugin_chrome-devtools-mcp_chrome-devtools, but never "__").
  const m = n.match(/^mcp__(.+?)__(.+)$/);
  return m ? mcpSanitizeKey(m[1]) : DEFAULT_MCP_SERVER_KEY;
}

// mcpToolsForServer returns the slice of the session's advertised tools that belongs to serverKey under the
// active grouping. For grouping "one" it returns ALL advertised tools. In natural grouping, an omitted HTTP
// path key means the generic client-tools bucket; it must not duplicate tools from named MCP server buckets. Recomputed
// per request (never cached) because session.advertise can change per turn. Each entry is shaped for
// tools/list: {name, description, inputSchema} with a valid object inputSchema default.
function mcpToolsForServer(session, serverKey, grouping = MCP_GROUPING) {
  // ADD-40: tools/list during a live run reflects the turn-scoped effective set (tool_choice-gated), so a
  // none/specific run does not even ADVERTISE a disallowed tool through the shim.
  const adv = (session && session.advertiseForGating && session.advertiseForGating()) || (session && session.advertise) || [];
  const effectiveServerKey = serverKey || DEFAULT_MCP_SERVER_KEY;
  const all = grouping === "one";
  const configuredKeys = session && Array.isArray(session.mcpServerKeys)
    ? new Set(session.mcpServerKeys)
    : null;
  const out = [];
  for (const t of adv) {
    const name = t.toolName || t.name;
    if (!name) continue;
    let assignedServerKey = mcpServerKeyForTool(name, grouping);
    // The SDK agent's mcpServers map is fixed for the lifetime of its current
    // handle. Route tools from a newly appearing natural/per-tool server over
    // the always-dialed generic server instead of making them disappear.
    if (!all && configuredKeys && !configuredKeys.has(assignedServerKey)) assignedServerKey = DEFAULT_MCP_SERVER_KEY;
    if (!all && assignedServerKey !== effectiveServerKey) continue;
    const schema = t.inputSchema && typeof t.inputSchema === "object" ? t.inputSchema : { type: "object" };
    // Inject a schema-derived argument contract (+ any per-tool extra) so the model calls each tool with its exact
    // arg shape and never conflates tools. ONLY in what composer reads here; session.advertise stays untouched.
    out.push({ name, description: augmentToolDescription(name, t.description || "", schema), inputSchema: schema });
  }
  return out;
}

function mcpDescriptorsForServer(session, serverKey, grouping = MCP_GROUPING) {
  const listedNames = new Set(mcpToolsForServer(session, serverKey, grouping).map((tool) => tool.name));
  const advertised = (session && session.advertiseForGating && session.advertiseForGating())
    || (session && session.advertise)
    || [];
  return advertised.filter((tool) => listedNames.has(tool.toolName || tool.name));
}

// buildMcpServers returns the Record<serverKey, McpServerConfig> registered via AgentOptions.mcpServers for a
// session, or {} when the shim is off (DEFAULT ON; off only when CURSOR_COMPOSER_MCP_SHIM is "0"/"false").
// Every server is the same loopback http shape; only the SET of keys + the per-key tool slice differ by
// grouping. The url carries the sessionId (authoritative) and, when grouping != "one", the serverKey segment;
// a belt-and-suspenders generic client-tools header is sent too (our handler ignores it — the path is authoritative).
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
      url: `http://127.0.0.1:${PORT}/mcp/${sid}` + (serverKey && serverKey !== DEFAULT_MCP_SERVER_KEY ? `/${serverKey}` : ""),
      headers: { "X-Client-Tools-Session": sid },
    });
    // Comment 6: register at least ONE session-scoped loopback server even when the CURRENT turn advertises NO
    // tools. The durable agent's mcpServers map is fixed when the agent is first created/resumed; if we returned
    // {} on a tool-less first turn, the SDK would never dial /mcp and a tool advertised on a LATER turn could not
    // surface (tools/list reads session.advertise DYNAMICALLY, so the empty server still picks up later tools
    // without rotating the durable agent). Always-register is the simpler path (no advertise-transition rotation).
    if (!adv.length) {
      dbg("buildMcpServers: no tools this turn -> register one empty generic session server (Comment 6)", "session=" + sid);
      return { [DEFAULT_MCP_SERVER_KEY]: mkServer(DEFAULT_MCP_SERVER_KEY) };
    }
    if (MCP_GROUPING === "one") return { [DEFAULT_MCP_SERVER_KEY]: mkServer(DEFAULT_MCP_SERVER_KEY) };
    // Always dial the generic server. Besides hosting ordinary harness tools,
    // it is the overflow route for a natural/per-tool server that appears on a
    // later turn after the SDK agent's server map has become fixed.
    const servers = Object.create(null);
    servers[DEFAULT_MCP_SERVER_KEY] = mkServer(DEFAULT_MCP_SERVER_KEY);
    // R5 collision guard (natural only): detect two distinct server tokens collapsing to one URL-safe key.
    const seenRaw = new Map(); // sanitizedKey -> rawServerToken (for the collision check)
    for (const t of adv) {
      const name = t.toolName || t.name;
      if (!name) continue;
      const key = mcpServerKeyForTool(name, MCP_GROUPING);
      if (MCP_GROUPING === "natural") {
        const m = name.match(/^mcp__(.+?)__(.+)$/);
        const raw = m ? m[1] : DEFAULT_MCP_SERVER_KEY;
        const prev = seenRaw.get(key);
        if (prev !== undefined && prev !== raw) {
          dbg("buildMcpServers natural key collision -> degrade to one", "session=" + sid, "key=" + key, "a=" + prev, "b=" + raw);
          return { [DEFAULT_MCP_SERVER_KEY]: mkServer(DEFAULT_MCP_SERVER_KEY) };
        }
        seenRaw.set(key, raw);
      }
      if (!Object.hasOwn(servers, key)) servers[key] = mkServer(key);
    }
    return servers;
  } catch (e) {
    dbg("buildMcpServers threw (fail closed)", "session=" + (session && session.id), (e && e.message) || String(e));
    throw e;
  }
}

// headlessMcpState answers the SDK runtime's mcp_state_exec CLIENT request — the headless equivalent of the
// Cursor IDE reporting its MCP inventory. The runtime feeds the model its MCP toolset from THIS reply
// (mcpStateAccessor.getState); answering it with a typed-unavailable error (the old TYPED_UNAVAILABLE_U
// behavior) made the backend expose ZERO MCP tools even though the loopback servers were dialed + tools/list'd,
// so composer never invoked an advertised MCP tool (observed dispatchMcp=0, no tools/call). We report each
// DIALED loopback server (buildMcpServers is the authoritative set — same keys, INCL. the Comment-6
// always-register generic server and the natural collision-degrade) with its currently-enabled tool slice
// (mcpToolsForServer is tool_choice-gated, ADD-40). server_identifier == the dialed server key so the runtime
// correlates a state server to its dialed counterpart; tool name == tool_name and the provider identifier
// matches the run-request mcp_tools advertise so the backend treats them as the SAME tool; status:"connected" is
// the runtime's "ready" value (a "needsAuth" server is filtered out). Fail-safe: empty/absent advertise (or
// shim off) -> { servers: [] }, an HONEST "no servers" success (never a fabricated tool, strictly better than
// the old error); any throw falls back to the typed-unavailable error (no worse than before). HANDLER change
// only — the exec was already routed to __CC_EXEC_U by the patch, exactly like requestContextArgs.
function headlessMcpState(session) {
  try {
    const dialed = Array.isArray(session && session.mcpServerKeys)
      ? Object.fromEntries(session.mcpServerKeys.map((key) => [key, true]))
      : buildMcpServers(session); // before agent creation, derive the exact set that will be dialed
    const servers = [];
    for (const key of Object.keys(dialed)) {
      const tools = mcpToolsForServer(session, key).map((t) => ({
        name: t.name,
        toolName: t.name, // dispatchMcp reconciles by name; keep tool_name == name
        providerIdentifier: CLIENT_TOOL_PROVIDER_ID,
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
        dbg("mcp initialize", "session=" + sessionId, "serverKey=" + (serverKey || DEFAULT_MCP_SERVER_KEY), "protocol=" + ver);
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
        dbg("mcp tools/list", "session=" + sessionId, "serverKey=" + (serverKey || DEFAULT_MCP_SERVER_KEY), "count=" + tools.length);
        dbgToolFormat("tools/list", sessionId, serverKey, tools); // verbose: the exact per-tool schema the model receives
        // Return everything; omit nextCursor (a few hundred tools is fine, no real pagination needed).
        return mcpResult(id, { tools });
      }
      case "tools/call": {
        const want = (params && params.name) || "";
        const input = (params && params.arguments) || {};
        const session = sessions.get(sessionId);
        dbg("mcp tools/call", "session=" + sessionId, "serverKey=" + (serverKey || DEFAULT_MCP_SERVER_KEY), "name=" + want);
        if (!session) {
          // Degrade, never fake success: an unknown/expired session yields a typed isError tool result.
          return mcpResult(id, { content: [{ type: "text", text: `session ${sessionId} not found (bridge restart or idle eviction); the tool call cannot be routed` }], isError: true });
        }
        const scopedTools = mcpDescriptorsForServer(session, serverKey);
        const ccName = resolveClientToolName(want, scopedTools);
        dbg("mcp tools/call reconciled", "session=" + sessionId, "want=" + want, "reconciled=" + (ccName || "<UNAVAILABLE>"));
        if (!ccName) {
          const names = scopedTools.map((t) => t.toolName || t.name).join(", ");
          session.recordInternalToolRejection("http-mcp-unmatched", want, { settleAnnounced: true, input });
          return mcpResult(id, { content: [{ type: "text", text: `Tool '${want}' is not available. Available tools: ${names || "(none)"}.` }], isError: true });
        }
        try {
          const effectiveServerKey = serverKey || DEFAULT_MCP_SERVER_KEY;
          const rpcId = id == null ? randomUUID() : Buffer.from(JSON.stringify(id)).toString("base64url");
          const out = await session.openClientTool({
            source: "http-mcp",
            rawToolCallId: `http-mcp:${effectiveServerKey}:${rpcId}`,
            name: ccName,
            input,
            resultAdapter: (result) => result,
          });
          // Normalize once via shared helper (checklist 3)
          const norm = normalizeClientToolResult(
            out.content,
            out.isError,
            out.images,
            out.structuredContent,
            ccName,
            out.contentBlocks,
            out.structuredContentPresent,
          );
          session.rememberLargeMcpResult(norm.content, ccName);
          const parts = [];
          for (const block of norm.contentBlocks) {
            if (block && block.type === "text") parts.push({ type: "text", text: block.text });
            else if (block && block.type === "json") parts.push({ type: "text", text: JSON.stringify(block.value) });
            else if (block && block.type === "image" && block.source && block.source.kind === "base64" && mcpImageResultsEnabled()) {
              parts.push({ type: "image", data: block.source.data, mimeType: block.source.mimeType });
            } else if (block && block.type === "image" && block.source && block.source.kind === "url") {
              parts.push({
                type: "resource_link",
                uri: block.source.url,
                name: "tool-result image",
                ...(block.source.mimeType ? { mimeType: block.source.mimeType } : {}),
              });
            }
          }
          if (parts.length === 0) parts.push({ type: "text", text: "" });
          const result = { content: parts, isError: norm.isError };
          const structured = mcpStructuredProjection(norm.structuredContent, norm.structuredContentPresent);
          if (structured.structuredContent !== undefined) result.structuredContent = structured.structuredContent;
          if (structured.marker !== null) result.content.push({ type: "text", text: structured.marker });
          return mcpResult(id, result);
        } catch (rejErr) {
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

async function dispatchMcpBatch(messages, sessionId, serverKey) {
  const responses = [];
  // A JSON-RPC batch permits concurrent execution, but real agent clients
  // routinely send initialize/activation/call sequences in one array. Ordered
  // dispatch preserves those causal semantics and matches the pre-redesign
  // behavior; batches are small enough that correctness dominates latency.
  for (const message of messages) {
    const response = await mcpDispatch(message, sessionId, serverKey);
    if (response !== null) responses.push(response);
  }
  return responses;
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
    if (body.length === 0) {
      res.writeHead(200, { "Content-Type": "application/json" });
      res.end(JSON.stringify(mcpError(null, -32600, "Invalid Request")));
      return;
    }
    const out = await dispatchMcpBatch(body, sessionId, serverKey);
    // A batch of pure notifications yields no responses -> 202; otherwise return the response array.
    if (!out.length) { res.writeHead(202, { "Content-Type": "application/json" }); res.end(); return; }
    res.writeHead(200, { "Content-Type": "application/json" }); res.end(JSON.stringify(out)); return;
  }
  const result = await mcpDispatch(body, sessionId, serverKey);
  if (result === null) { res.writeHead(202, { "Content-Type": "application/json" }); res.end(); return; }
  res.writeHead(200, { "Content-Type": "application/json" }); res.end(JSON.stringify(result));
}

function streamCallbacks(session, ep, onUserMessageAppended = null) {
  // #13: EPOCH-GATE the producer. A superseded/cancelled run (cancel() bumps runEpoch) or a completed session
  // must never write text/reasoning into a SUCCESSOR turn's activeRes or mutate its buffers — that is the
  // cross-run output leak. ep is captured at agent.send; every callback no-ops once the epoch advances or the
  // session is done. (The completion callback at run.wait() is already epoch-gated the same way.)
  const live = () => ep === session.runEpoch && !session.done;
  return {
    onDelta: ({ update }) => {
      try {
        if (!live()) return; // #13: drop a dead/superseded run's late delta — never leak into a successor turn
        session.lastSdkActivityAt = nowMs();
        const ty = update && (update.type || update.case);
        const txt = update && (update.text != null ? update.text : (update.value && update.value.text));
        if (ty === "user-message-appended") {
          if (typeof onUserMessageAppended === "function") onUserMessageAppended(update.userMessage || null);
        } else if (ty === "text-delta" && txt) {
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
            "toolBatch=" + session.activeToolBatch().length, "queued=" + session.queuedToolCalls().length, "activeRes=" + !!session.activeRes); } catch { /* never throw from a probe */ }
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
        session.lastSdkActivityAt = nowMs();
        dbg("onStep PROBE", "session=" + session.id, "toolBatch=" + session.activeToolBatch().length,
          "queued=" + session.queuedToolCalls().length, "activeRes=" + !!session.activeRes,
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
    dbg(tag + " TOOLFMT", "session=" + sessionId, "serverKey=" + (serverKey || DEFAULT_MCP_SERVER_KEY), "n=" + parts.length, "bareSchema=" + bareN, parts.join(" "));
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

// Compatibility exports for callers/tests that used the pre-adapter helpers.
// Production calls use ToolContractRegistry.normalize so scalars, arrays,
// objects, aliases, and client-only decorations share one implementation.
function extractScalarFromWrapper(obj, wantType, sameKey) {
  const key = sameKey || "value";
  const schema = { type: "object", properties: { [key]: { type: wantType } } };
  const normalized = normalizeToolArguments({ [key]: obj }, schema).value[key];
  return normalized === obj ? undefined : normalized;
}

function normalizeToolArgsToSchema(_name, input, schema) {
  return normalizeToolArguments(input, schema).value;
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

// CURSOR_COMPOSER_USER_MSG_REMINDER (default empty = off): when set, its text is included as standing context on
// every non-empty user turn. The current user instruction is deliberately rendered AFTER the reminder. A hidden
// harness reminder must never become the final model-visible instruction and silently outrank the user's words.
const USER_MSG_REMINDER = (process.env.CURSOR_COMPOSER_USER_MSG_REMINDER || "").trim();
function appendRulesReminder(userText, reminder = USER_MSG_REMINDER) {
  if (typeof userText !== "string" || userText.length === 0) return userText;
  const parts = [];
  if (reminder) {
    parts.push("[Standing tool-use context; this is reference material, not the user's request]\n" + reminder);
  }
  parts.push("[CURRENT USER MESSAGE — authoritative input for this turn]\n"
    + "Follow its literal intent. An explicit correction or replacement supersedes conflicting prior work; "
    + "a continuation or clarification keeps relevant unfinished work.\n" + userText);
  return parts.join("\n\n");
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
  if (isLoopbackRemote(req) || authed) return { ok: true, ready: bridgeReady, patched: true, sessions: sessions.size };
  return { ok: true };
}

function readinessBody() {
  if (!bridgeReady) return { ok: false, ready: false };
  if (!stateRootDiskStatus().ok) {
    return { ok: false, ready: false, reason: "durable_state_capacity" };
  }
  return { ok: true, ready: true };
}

function classifyMcpRoute(req) {
  const requestUrl = String(req && req.url || "");
  if (!(requestUrl === "/mcp" || requestUrl.startsWith("/mcp/"))) return null;
  if (!isLoopbackRemote(req)) {
    return { error: "the SDK MCP shim is loopback-only", status: 403 };
  }
  const segs = requestUrl.split("?", 1)[0].split("/").filter(Boolean);
  if (segs.length > 3) return { error: "malformed MCP path", status: 400 };
  try {
    const sessionId = segs[1] ? decodeURIComponent(segs[1]) : "";
    const serverKey = segs[2] ? decodeURIComponent(segs[2]) : "";
    if (!sessionId) return { error: "missing sessionId in /mcp path", status: 404 };
    return { sessionId, serverKey };
  } catch {
    return { error: "malformed MCP path encoding", status: 400 };
  }
}

// PayloadTooLargeError marks a request body that exceeded MAX_AGENT_TURN_BYTES so callers can map it to 413.
class PayloadTooLargeError extends Error { constructor(msg) { super(msg); this.code = "PAYLOAD_TOO_LARGE"; } }
// readBodyBounded reads a request body but stops + throws once the accumulated BYTE length exceeds
// MAX_AGENT_TURN_BYTES (M26), so a runaway body can never be fully buffered into memory. Byte length (not
// char length) is tracked so multibyte/base64 payloads are bounded accurately. Returns the raw string.
async function readBodyBounded(req) {
  const chunks = [];
  let bytes = 0;
  for await (const c of req) {
    const chunk = Buffer.isBuffer(c) ? c : Buffer.from(c);
    bytes += chunk.length;
    if (bytes > MAX_AGENT_TURN_BYTES) {
      throw new PayloadTooLargeError(`request body exceeds MAX_AGENT_TURN_BYTES (${MAX_AGENT_TURN_BYTES} bytes)`);
    }
    chunks.push(chunk);
  }
  // HTTP chunk boundaries are arbitrary and may split one UTF-8 code point.
  // Decoding each chunk independently inserts U+FFFD, which changes the
  // byte-exact tool inventory snapshot and used to create a false epoch 422.
  // Concatenate bytes first and reject genuinely invalid UTF-8 instead of
  // silently repairing it into different JSON.
  return new TextDecoder("utf-8", { fatal: true }).decode(Buffer.concat(chunks, bytes));
}

async function writeShortSse(res, terminal, leaseSource = null, leaseTerminal = false) {
  res.writeHead(200, SSE_HEADERS);
  const writer = new SseWriter(res, { maxQueueBytes: COMPOSER_OUT_QUEUE_MAX_BYTES });
  const receipt = writer.write(formatSseData(
    leaseSource ? withClientLease(terminal, leaseSource, leaseTerminal) : terminal,
  ));
  if (receipt.ok) await receipt.handedToNode.catch(() => {});
  await writer.endAfter("data: [DONE]\n\n");
}

// A stateless harness may retry the same mixed continuation while the first
// HTTP response is still inside agent.send(). Replace the abandoned response
// sink and keep streaming the SAME SDK turn instead of returning a permanent
// 409 or starting a duplicate send. The turn token prevents the handoff from
// waiting on a later turn that happens to reuse the Session.
async function handoffActiveTurn(req, res, session, round, committed, clientMessageId) {
  const token = session.turnToken;
  res.writeHead(200, SSE_HEADERS);
  const previousWriter = session.responseWriter;
  const writer = session.beginResponse(res);
  if (previousWriter && previousWriter !== writer) {
    // beginResponse installed the replacement first, so the prior writer's
    // failure callback observes that it no longer owns the Session and cannot
    // cancel the live run.
    previousWriter.fail(new Error("response superseded by an idempotent retry"));
  }
  session.writePayload(formatSseData({
    type: "session",
    sessionId: session.id,
    continuationReceipt: {
      ...(round
        ? continuationReceipt(round, committed, {
          clientMessageId,
          userInputStatus: String(round.deferredInput(clientMessageId)?.state || "").toLowerCase(),
        })
        : { contractVersion: CLIENT_TOOL_CONTRACT_VERSION, clientMessageId, userInputStatus: "active" }),
      receipt: "active_turn_handoff",
    },
  }));
  // Rebuild the replacement stream from the exact ordered frames already
  // accepted by the prior response. A cumulative text fallback loses
  // reasoning/tool ordering and can duplicate text when later deltas were
  // buffered after disconnect. replayEvents is the same bounded canonical log
  // that becomes the durable completion receipt.
  for (const event of session.replayEvents) {
    session.writePayload(formatSseData(event));
  }
  if (session.pendingDeltas.length > 0) session.flushPendingDeltas();
  else if (session.replayEvents.length === 0 && session.streamedText) {
    // Compatibility for a pre-replay-log in-memory Session created before an
    // in-process upgrade. New runs always record text in replayEvents.
    session.writePayload(formatSseData({ type: "text", delta: session.streamedText }));
  }

  let closed = false;
  const onClose = () => {
    if (closed || session.activeRes !== res) return;
    session.failWrite("replacement response closed");
  };
  res.on("close", onClose);
  try {
    await session.whenTurnSettled(token);
  } finally {
    closed = true;
    res.off("close", onClose);
    if (session.activeRes === res) {
      try { await session.finishResponse(); }
      finally { session.activeRes = null; session.responseWriter = null; }
    }
  }
}

// If the original fresh-turn response was lost after its tool calls were
// durably handed off, replay those same signed calls. This is a response replay,
// not a second SDK send: callbacks and watchdog ownership remain on the
// existing ToolRound.
async function replayPausedFreshTurn(res, session, clientMessageId) {
  const round = session.currentRound;
  const calls = round ? round.fifo.map((id) => round.calls.get(id)).filter((call) =>
    call && call.state === CallState.HANDED_TO_TRANSPORT && !call.receipt) : [];
  if (calls.length === 0) return false;
  res.writeHead(200, SSE_HEADERS);
  const writer = new SseWriter(res, { maxQueueBytes: COMPOSER_OUT_QUEUE_MAX_BYTES });
  const write = async (value) => {
    const receipt = writer.write(formatSseData(value));
    if (!receipt.ok) throw new Error("paused-turn replay transport failed");
    await receipt.handedToNode;
  };
  await write({
    type: "session",
    sessionId: session.id,
    continuationReceipt: {
      contractVersion: CLIENT_TOOL_CONTRACT_VERSION,
      clientMessageId,
      receipt: "paused_turn_replay",
    },
  });
  if (session.replayEvents.length > 0) {
    for (const event of session.replayEvents) await write(event);
  } else {
    // Compatibility for an in-memory Session created before ordered replay
    // logging existed. New sessions never take this lossy fallback.
    if (session.streamedText) await write({ type: "text", delta: session.streamedText });
    for (const call of calls) {
      await write({ type: "tool_call", id: call.wireId, name: call.name, input: call.input });
    }
  }
  await write(withClientLease(
    { type: "turn_end", stop_reason: "tool_use", tool_calls: calls.map((call) => call.wireId) },
    session,
    false,
  ));
  await writer.endAfter("data: [DONE]\n\n");
  return true;
}

async function replayCompletedTurn(res, receipt) {
  res.writeHead(200, SSE_HEADERS);
  const writer = new SseWriter(res, { maxQueueBytes: COMPOSER_OUT_QUEUE_MAX_BYTES });
  const write = async (value) => {
    const handed = writer.write(formatSseData(value));
    if (!handed.ok) throw new Error("completed-turn replay transport failed");
    await handed.handedToNode;
  };
  await write({
    type: "session",
    sessionId: receipt.sessionId,
    continuationReceipt: {
      contractVersion: CLIENT_TOOL_CONTRACT_VERSION,
      clientMessageId: receipt.clientMessageId,
      receipt: "completed_turn_replay",
    },
  });
  const events = receipt.version >= 4
    ? canonicalReplayEvents(receipt.events)
    : [
      ...(receipt.text ? [{ type: "text", delta: receipt.text }] : []),
      { type: "turn_end", stop_reason: "end_turn", status: "finished", usage: receipt.usage || {} },
    ];
  for (const event of events) {
    if (event.type === "turn_end") {
      await write(withClientLease(event, receipt, true));
      continue;
    }
    if ((event.type === "text" || event.type === "reasoning") && event.delta.length > 32768) {
      // Preserve event order while keeping individual replay frames small and
      // never splitting a UTF-16 surrogate pair.
      for (let offset = 0; offset < event.delta.length;) {
        let end = Math.min(event.delta.length, offset + 32768);
        if (end < event.delta.length
            && event.delta.charCodeAt(end - 1) >= 0xD800 && event.delta.charCodeAt(end - 1) <= 0xDBFF
            && event.delta.charCodeAt(end) >= 0xDC00 && event.delta.charCodeAt(end) <= 0xDFFF) end--;
        await write({ type: event.type, delta: event.delta.slice(offset, end) });
        offset = end;
      }
      continue;
    }
    await write(event);
  }
  await writer.endAfter("data: [DONE]\n\n");
}

function continuationFailure(res, status, code, message, details = null) {
  res.writeHead(status, { "Content-Type": "application/json" });
  res.end(JSON.stringify({ error: { code, message, ...(details ? { details } : {}) } }));
}

async function finishStartedSseWithError(res, {
  receipt = "request_failure_after_headers",
  retryable = false,
  retryMode = "",
  error = "request failed after the response stream started",
} = {}) {
  if (!res || res.writableEnded || res.destroyed) return false;
  const contentType = String(res.getHeader && res.getHeader("Content-Type") || "").toLowerCase();
  if (!contentType.includes("text/event-stream")) {
    try { res.end(); } catch {}
    return false;
  }
  try {
    res.write(formatSseData({
      type: "turn_end",
      stop_reason: "error",
      receipt,
      retryable,
      ...(retryMode ? { retryMode } : {}),
      error,
    }));
    res.end("data: [DONE]\n\n");
    return true;
  } catch {
    try { res.destroy(); } catch {}
    return false;
  }
}

function durableRecoveryResults(round, input) {
  // Validate that every result named by this request crossed the durable
  // receipt boundary. The recovery payload itself is then rebuilt from ALL
  // receipts in call order, not merely from this HTTP projection: clients may
  // submit parallel results incrementally and omit already-acknowledged
  // siblings from the final request.
  for (const submitted of input.results || []) {
    const call = round.calls.get(submitted.toolCallId);
    if (!call || !call.receipt || !call.receipt.result) {
      throw new ToolRoundError(
        "missing_durable_receipt",
        `tool result ${submitted.toolCallId || "<missing>"} has no durable recovery receipt`,
        500,
      );
    }
  }
  const recovered = [];
  for (const id of round.fifo) {
    const call = round.calls.get(id);
    if (!call || !call.receipt || !call.receipt.result) continue;
    recovered.push({ toolCallId: id, ...call.receipt.result });
  }
  return recovered;
}

function recoveryResultSummary(round, input) {
  return durableRecoveryResults(round, input).map((result) => {
    const call = round.calls.get(result.toolCallId);
    const images = Array.isArray(result.images)
      ? result.images.map((image) => ({
        mimeType: image && image.mimeType,
        url: image && image.url,
        detail: image && image.detail,
        attached: !!(image && image.data),
      }))
      : [];
    return {
      toolCallId: result.toolCallId,
      toolName: call ? call.name : "",
      content: result.content,
      ...(Array.isArray(result.contentBlocks) ? {
        contentBlocks: result.contentBlocks.map((block) => {
          if (block && block.type === "image") {
            const image = block.image || block.source || block;
            return {
              type: "image",
              mimeType: image && (image.mimeType || image.media_type),
              url: image && image.url,
              detail: image && image.detail,
              attached: !!(image && image.data),
            };
          }
          return block;
        }),
      } : {}),
      isError: result.isError === true,
      structuredContentPresent: result.structuredContentPresent === true
        || Object.prototype.hasOwnProperty.call(result, "structuredContent"),
      ...(result.structuredContentPresent === true || Object.prototype.hasOwnProperty.call(result, "structuredContent")
        ? { structuredContent: result.structuredContent ?? null }
        : {}),
      images,
    };
  });
}

function buildResultRecoveryInput(round, input, { requireHistory = false } = {}) {
  const history = typeof input.history === "string" ? input.history.trim() : "";
  const currentUser = typeof input.userText === "string" && input.userText.trim()
    ? input.userText
    : (input.type === "user" && typeof input.text === "string" ? input.text.trim() : "");
  if (requireHistory && !history && !currentUser) return null;
  const summary = recoveryResultSummary(round, input);
  const receiptedInput = { ...input, results: durableRecoveryResults(round, input) };
  const trailing = typeof input.userText === "string"
    ? input.userText
    : (input.type === "user" && typeof input.text === "string" ? input.text : "");
  const recoveryContext = [
    "[Recovered client-tool results — reference context]",
    "These are the exact results returned by the user-machine harness for tool calls already present in the prior conversation:",
    JSON.stringify(summary),
    trailing
      ? "Treat these results as reference-only facts. Follow the separately framed current user message according to its literal intent; explicit corrections replace conflicting prior work, while continuation requests preserve relevant unfinished work."
      : "Use these results only to continue the unfinished work they directly support.",
  ].join("\n");
  const instruction = trailing
    || "Continue only the unfinished work directly supported by the recovered client-tool results.";
  return {
    ...input,
    type: "user",
    recoveryContext,
    text: instruction,
    images: [...(Array.isArray(input.images) ? input.images : []), ...collectToolResultImages(receiptedInput)],
  };
}

function buildRestartRecoveryInput(round, input) {
  const context = round && round.recoveryContext && typeof round.recoveryContext === "object"
    ? round.recoveryContext : {};
  const merged = {
    ...input,
    history: typeof input.history === "string" && input.history.trim()
      ? input.history : (typeof context.history === "string" ? context.history : ""),
    historyFingerprint: typeof input.historyFingerprint === "string" && input.historyFingerprint
      ? input.historyFingerprint
      : (typeof context.historyFingerprint === "string" ? context.historyFingerprint : ""),
    system: Object.prototype.hasOwnProperty.call(input, "system")
      ? input.system : (typeof context.system === "string" ? context.system : ""),
  };
  if (Array.isArray(input.systemBlocks)) merged.systemBlocks = input.systemBlocks;
  else if (Array.isArray(context.systemBlocks)) merged.systemBlocks = context.systemBlocks;
  if (Array.isArray(input.images) && input.images.length > 0) merged.images = input.images;
  else if (Array.isArray(context.images) && context.images.length > 0) merged.images = context.images;
  if (!(typeof merged.userText === "string" && merged.userText.trim())
      && !(merged.type === "user" && typeof merged.text === "string" && merged.text.trim())
      && typeof context.currentUser === "string" && context.currentUser.trim()) {
    merged.userText = context.currentUser;
  }
  return buildResultRecoveryInput(round, merged, { requireHistory: true });
}

function systemContextPlan(session, input) {
  const blocks = normalizeSystemBlocks(input);
  if (blocks === null) {
    const incoming = typeof input?.system === "string" ? input.system : "";
    if (!incoming) return { kind: "none", blocks: null, ids: null, text: "" };
    if (!session.seeded) return { kind: "seed", blocks: null, ids: null, text: incoming };
    if (incoming === session.seededSystem) return { kind: "same", blocks: null, ids: null, text: "" };
    // Compatibility for older direct bridge callers that supplied only one
    // opaque aggregate. Without block identity the bridge cannot prove
    // replacement versus append, so preserve the historical update behavior.
    // Current Go clients always send structured blocks and take the safe path.
    return { kind: "legacy_update", blocks: null, ids: null, text: incoming };
  }
  const ids = blocks.map((block) => block.id);
  if (!session.seeded) {
    return { kind: "seed", blocks, ids, text: blocks.map((block) => block.text).join("\n\n") };
  }
  if (sameStringArray(session.seededSystemBlockIds, ids)) {
    return session.seededSystem === String(input.system || "")
      ? { kind: "same", blocks, ids, text: "" }
      : { kind: "replace", blocks, ids, text: blocks.map((block) => block.text).join("\n\n") };
  }
  if (session.seededSystemBlockIds.length > 0
      && isStringArrayPrefix(session.seededSystemBlockIds, ids)) {
    const suffix = blocks.slice(session.seededSystemBlockIds.length);
    return { kind: "append", blocks, ids, text: suffix.map((block) => block.text).join("\n\n") };
  }
  // Backward-compatible migration for an in-process session seeded before
  // structured block identity existed. Exact aggregate equality proves this
  // request is not changing model context; future turns use block identity.
  if (session.seededSystemBlockIds.length === 0
      && session.seededSystem
      && session.seededSystem === String(input.system || "")) {
    return { kind: "same", blocks, ids, text: "" };
  }
  return {
    kind: "replace",
    blocks,
    ids,
    text: blocks.map((block) => block.text).join("\n\n"),
  };
}

function toolResultRecoveryPlan(round, input, session) {
  const durableResults = durableRecoveryResults(round, input);
  const resultImages = collectToolResultImages({ results: durableResults });
  const fallbackImages = mcpImageResultsEnabled()
    ? resultImages.filter((image) => !(image && typeof image.data === "string" && image.data))
    : resultImages;
  const hasUserText = typeof input.userText === "string" && input.userText.length > 0;
  const hasUserImages = Array.isArray(input.images) && input.images.length > 0;
  const contextPlan = systemContextPlan(session, input);
  const systemChanged = contextPlan.kind === "append"
    || contextPlan.kind === "replace"
    || contextPlan.kind === "legacy_update";
  return {
    durableResults,
    resultImages,
    fallbackImages,
    hasUserText,
    hasUserImages,
    systemChanged,
    remainingUnreceipted: round.unreceiptedOwedCallCount,
    requiresFreshRecovery: fallbackImages.length > 0 || systemChanged || hasUserText || hasUserImages,
  };
}

function continuationHasDeferredInput(input) {
  return !!input && ((typeof input.userText === "string" && input.userText.length > 0)
    || (Array.isArray(input.images) && input.images.length > 0));
}

function deferredUserInput(round, item, input) {
  const stored = item && item.input ? item.input : {};
  const context = round && round.recoveryContext ? round.recoveryContext : {};
  return {
    ...input,
    type: "user",
    text: typeof stored.text === "string" ? stored.text : "",
    images: Array.isArray(stored.images) ? stored.images : [],
    system: typeof context.system === "string" ? context.system : "",
    ...(Array.isArray(context.systemBlocks) ? { systemBlocks: context.systemBlocks } : {}),
    history: typeof context.history === "string" ? context.history : "",
    historyFingerprint: typeof context.historyFingerprint === "string" ? context.historyFingerprint : "",
  };
}

function continuationReceipt(round, committed, extra = {}) {
  const resultDispositions = committed && Array.isArray(committed.dispositions) ? committed.dispositions : [];
  const toolEpochState = round.toolEpochState();
  return {
    contractVersion: CLIENT_TOOL_CONTRACT_VERSION,
    resultDispositions,
    acknowledgedToolResultIds: resultDispositions.map((result) => result.toolCallId),
    toolEpoch: toolEpochState,
    toolEpochState,
    ...(committed && committed.inventoryWarning ? {
      toolInventory: { status: "rejected", error: committed.inventoryWarning },
    } : {}),
    ...(committed && committed.deferredReceipt ? {
      clientMessage: committed.deferredReceipt,
    } : {}),
    ...extra,
  };
}

function deferredErrorDetails(round, clientMessageId, details = null) {
  const item = round && clientMessageId ? round.deferredInput(clientMessageId) : null;
  return {
    ...(details || {}),
    ...(item ? {
      clientMessageId,
      userInputStatus: String(item.state || "").toLowerCase(),
    } : {}),
  };
}

function continuationTenantMismatch(round, cursorKey, multiTenant = MULTI_TENANT) {
  const stored = typeof round?.tenantFingerprint === "string" ? round.tenantFingerprint : "";
  if (multiTenant && stored === "") return true;
  return stored !== "" && stored !== keyFingerprint(cursorKey);
}

function legacyRecoveryKeyFor(sessionId, clientMessageId) {
  return "clr1_" + createHash("sha256")
    .update(String(sessionId || ""))
    .update("\0")
    .update(String(clientMessageId || ""))
    .digest("hex")
    .slice(0, 24);
}

// Before signed routing tokens existed, a harness-visible call id was an opaque `call_*` value. There is no
// durable ToolRound route to parse or callback to resolve after an upgrade/restart, but a stateless harness can
// still supply a faithful bounded replay containing the calls and completed results. Accept only an explicitly
// marked, entirely unsigned batch. Any signed/future-routed or mixed batch falls through to strict parsing.
function legacyUnsignedRecoveryBody(body, input, results) {
  if (!input || input.legacyUnsignedReplay !== true) return null;
  const ids = results.map((result) => typeof result?.toolCallId === "string" ? result.toolCallId.trim() : "");
  if (ids.some((id) => !id)
      || ids.some((id) => id.startsWith(`${ROUTING_TOKEN_VERSION}_`) || /^cct[0-9]+_/.test(id))) return null;

  const sessionId = typeof body?.sessionId === "string" ? body.sessionId.trim() : "";
  const clientMessageId = typeof input.clientMessageId === "string" ? input.clientMessageId.trim() : "";
  const history = typeof input.history === "string" ? input.history : "";
  if (!sessionId || !clientMessageId || !history.trim()) {
    throw new ToolRoundError(
      "legacy_recovery_unavailable",
      "unsigned legacy tool results require a stable client message id and bounded conversation replay",
      410,
    );
  }
  const missingIds = ids.filter((id) => !history.includes(id));
  if (missingIds.length > 0) {
    throw new ToolRoundError(
      "legacy_recovery_unavailable",
      "bounded conversation replay does not contain every unsigned legacy tool call/result",
      410,
      { missingToolCallIds: missingIds },
    );
  }
  const images = [
    ...(Array.isArray(input.images) ? input.images : []),
    ...collectToolResultImages(input),
  ];
  const userText = typeof input.userText === "string" && input.userText.trim()
    ? input.userText
    : "Continue from the recovered conversation. The completed client-tool results are present in the replayed history.";
  return {
    ...body,
    input: {
      type: "user",
      text: userText,
      history,
      ...(typeof input.historyFingerprint === "string" && input.historyFingerprint
        ? { historyFingerprint: input.historyFingerprint }
        : {}),
      ...(typeof input.system === "string" && input.system ? { system: input.system } : {}),
      ...(Array.isArray(input.systemBlocks) ? { systemBlocks: input.systemBlocks } : {}),
      ...(images.length > 0 ? { images } : {}),
      clientMessageId,
      ...(typeof input.invocationId === "string" ? { invocationId: input.invocationId } : {}),
      legacyRecoveryKey: legacyRecoveryKeyFor(sessionId, clientMessageId),
      legacyRecoveredToolCallIds: ids,
    },
  };
}

function validateCompletedContinuationPayload(results, cursorKey) {
  const { codec, journal } = getRoundInfrastructure();
  const grouped = new Map();
  for (const result of results) {
    const parsed = codec.parse(result && result.toolCallId);
    const group = grouped.get(parsed.route) || [];
    group.push(result);
    grouped.set(parsed.route, group);
  }
  for (const [route, group] of grouped) {
    const round = liveToolRounds.get(route) || ToolRound.load(journal, codec, route);
    // A completed answer receipt can outlive its terminal ToolRound journal.
    // Its full v4 requestHash still proves byte-independent semantic
    // equivalence. When the journal is present, however, validate against its
    // immutable result receipt before replay; never let preferDurableReceipt
    // silently convert a conflicting retry into the old answer.
    if (!round) continue;
    if (continuationTenantMismatch(round, cursorKey)) {
      throw new ToolRoundError("tenant_mismatch", "a signed tool round belongs to a different Cursor credential", 403);
    }
    round.preflightResults(group, {
      allowRegisteredReceipt: true,
      preferDurableReceipt: false,
    });
  }
}

// `/agent/continue` is the sole continuation ingress. It routes by the signed tool-call ids, validates and
// journals the complete batch before writing SSE headers, and only then settles the paused SDK callbacks.
async function handleContinue(req, res, body, cursorKey, casAttempt = 0, historicalDispositions = []) {
  const input = body && body.input;
  if (body && !validClientEnv(body.clientEnv)) body.clientEnv = { workspaceUnknown: true };
  let results = input && input.type === "tool_results" && Array.isArray(input.results) ? input.results : null;
  if (!results || results.length === 0) {
    continuationFailure(res, 422, "empty_tool_results", "a continuation requires at least one tool result");
    return;
  }
  let continuationIdentity;
  let continuationClientMessageHash;
  try {
    continuationIdentity = turnInvocationIdentity(input);
    // Optional representation defects are deliberately converted into the
    // same model-visible error result that ToolRound will commit. Hash that
    // authoritative accommodated payload, not the malformed projection that
    // would otherwise throw before the existing compatibility boundary.
    results = accommodateContinuationResults(results);
    continuationClientMessageHash = completedTurnRequestHash({ ...input, results });
  } catch (error) {
    continuationFailure(
      res,
      error instanceof ToolRoundError ? error.status : 422,
      error instanceof ToolRoundError ? error.code : "invalid_tool_results",
      (error && error.message) || String(error),
    );
    return;
  }
  const continuationClientMessageId = continuationIdentity.id;
  try {
    const legacyBody = legacyUnsignedRecoveryBody(body, input, results);
    if (legacyBody) {
      dbg("legacy unsigned continuation -> faithful deterministic recovery",
        "session=" + legacyBody.sessionId, "results=" + results.length,
        "clientMessageId=" + legacyBody.input.clientMessageId);
      return handleTurn(req, res, legacyBody, cursorKey);
    }
  } catch (error) {
    if (error instanceof ToolRoundError) {
      continuationFailure(res, error.status, error.code, error.message, error.details);
      return;
    }
    continuationFailure(res, 500, "legacy_recovery_failed", (error && error.message) || String(error));
    return;
  }
  let preparedToolRegistry;
  let inventoryWarning = "";
  let deferredRound = null;
  let deferredInputId = "";
  let uncertainDeliveryAgentId = "";
  let routedRound = null;
  try {
    // A replacement inventory governs the *next* model step; it is not part of
    // the already-executed tool result. Validate it independently so a client
    // adding or changing one malformed tool cannot strand an owed callback.
    preparedToolRegistry = prepareAdvertisedToolRegistry(body);
  } catch (error) {
    inventoryWarning = (error && error.message) || String(error);
    preparedToolRegistry = undefined;
    dbg("continuation replacement inventory rejected; preserving prior registry", inventoryWarning);
  }
  try {
    const { codec, journal } = getRoundInfrastructure();
    const parsed = results.map((result) => codec.parse(result && result.toolCallId));
    const route = parsed[0].route;
    if (parsed.some((token) => token.route !== route)) {
	  if (continuationIdentity.policy === TURN_IDENTITY_POLICY.INVOCATION_V1) {
	    throw new ToolRoundError(
	      "multi_route_invocation_ambiguous",
	      "one invocation identity cannot span multiple signed tool rounds; submit each round independently",
	      409,
	    );
	  }
      const grouped = new Map();
      for (let index = 0; index < parsed.length; index++) {
        const group = grouped.get(parsed[index].route) || [];
        group.push(results[index]);
        grouped.set(parsed[index].route, group);
      }
      const loaded = [...grouped].map(([groupRoute, groupResults]) => {
        const groupRound = liveToolRounds.get(groupRoute) || ToolRound.load(journal, codec, groupRoute);
        if (groupRound && continuationTenantMismatch(groupRound, cursorKey)) {
          throw new ToolRoundError("tenant_mismatch", "a signed tool round belongs to a different Cursor credential", 403);
        }
        return { groupRoute, groupResults, groupRound };
      });
      const present = loaded.filter(({ groupRound }) => !!groupRound);
      if (present.length === 0) {
        throw new ToolRoundError("round_lost", "none of the signed tool rounds remain in the durable journal", 410);
      }
      // Validate every route before mutating any journal. A stateless harness
      // may project old and current tool results into one request; terminal or
      // process-orphaned routes can be receipted independently and recovered
      // later. Only two *in-memory callback-owning* rounds are fundamentally
      // ambiguous because one HTTP response cannot carry two SDK continuations.
      for (const group of present) {
        group.groupRound.preflightResults(group.groupResults, {
          allowRegisteredReceipt: !liveToolRounds.has(group.groupRoute),
          preferDurableReceipt: true,
        });
      }
      const live = present.filter(({ groupRoute, groupRound }) =>
        groupRound.state !== RoundState.TERMINAL && liveToolRounds.get(groupRoute) === groupRound);
      if (live.length > 1) {
        await writeShortSse(res, {
          type: "turn_end",
          stop_reason: "error",
          receipt: "multiple_live_tool_rounds_deferred",
          retryable: true,
          retryMode: "split",
          error: "the request combines results from multiple simultaneously live conversations; no result was consumed, so each conversation can retry independently",
          routes: live.map(({ groupRoute }) => groupRoute),
        });
        return;
      }
      const active = present.filter(({ groupRound }) => groupRound.state !== RoundState.TERMINAL);
      const bodyMatches = active.filter(({ groupRound }) => body && body.sessionId === groupRound.sessionId);
      const primary = live[0] || (bodyMatches.length === 1 ? bodyMatches[0] : active.at(-1)) || present.at(-1);
      const acknowledged = [];
      for (const group of loaded) {
        if (group === primary) continue;
        if (!group.groupRound) {
          acknowledged.push(...group.groupResults.map((result) => ({
            toolCallId: result.toolCallId,
            status: "historical_route_absent_not_applied",
          })));
          continue;
        }
        const terminal = group.groupRound.state === RoundState.TERMINAL;
        const committedGroup = group.groupRound.commitResults(group.groupResults, {
          allowTerminalReceipt: terminal,
          allowRegisteredReceipt: true,
          preferDurableReceipt: true,
        });
        if (!terminal) {
          group.groupRound.terminalize(
            TerminalReason.RESTART_LOST,
            "tool result was receipted from a mixed-route stateless replay; a separate retry may faithfully resume it",
          );
          group.groupRound.recordRecovery({
            at: nowMs(),
            decision: "awaiting_separate_resume",
            reason: "one HTTP response can stream only the selected primary route",
          });
        }
        acknowledged.push(...committedGroup.dispositions.map((disposition) => ({
          ...disposition,
          status: terminal
            ? "historical_receipt_acknowledged"
            : "orphaned_route_receipted_for_separate_resume",
        })));
      }
      const nextBody = { ...body, input: { ...input, results: primary.groupResults } };
      return handleContinue(req, res, nextBody, cursorKey, casAttempt, [
        ...historicalDispositions,
        ...acknowledged,
      ]);
    }
    let round = liveToolRounds.get(route);
    const hasLiveCallbacks = !!round;
    if (!round) round = ToolRound.load(journal, codec, route);
    if (!round) throw new ToolRoundError("round_lost", "the signed tool round is not present in the durable journal", 410);
    routedRound = round;
    if (continuationTenantMismatch(round, cursorKey)) {
      throw new ToolRoundError("tenant_mismatch", "the signed tool round belongs to a different Cursor credential", 403);
    }
    // The signed ToolRound is authoritative for continuation routing. The
    // request body's sessionId is only advisory and may be the shared parent
    // conversation id used by concurrent Claude subagents. Return the routed
    // session over this authenticated bridge hop so Go can bind outward
    // response ids and previous_response_id mappings to the actual child.
    res.setHeader("X-CLIProxy-Composer-Session", round.sessionId);
    const liveRoundSession = sessions.get(round.sessionId);
    if (liveRoundSession) liveRoundSession.clientLeaseToken = round.clientLeaseToken;

    const wasTerminal = round.state === RoundState.TERMINAL;
    const hasDeferredInput = continuationHasDeferredInput(input);
    if (hasDeferredInput) {
      deferredRound = round;
      deferredInputId = continuationClientMessageId;
    }
    // Validate and bind the authoritative prospective payload before any
    // journal mutation. Existing immutable receipts win over a conflicting
    // stateless projection; an explicit invocation id can bind to only this
    // one canonical request.
    const prospective = round.preflightResults(results, {
      allowTerminalReceipt: !hasLiveCallbacks && wasTerminal,
      allowRegisteredReceipt: true,
      preferDurableReceipt: true,
    });
    const prospectiveById = new Map(prospective.map((entry) => [entry.call.wireId, entry]));
    const prospectiveAuthoritativeResults = results.map((result) => {
      const prepared = prospectiveById.get(result && result.toolCallId);
      return prepared ? { toolCallId: result.toolCallId, ...prepared.result } : result;
    });
    const prospectiveConflicts = prospective
      .filter((entry) => entry.durableReceiptRetained)
      .map((entry) => entry.call.wireId);
    continuationClientMessageHash = completedTurnRequestHash({
      ...input,
      results: prospectiveAuthoritativeResults,
    });
    bindInvocationRequestHash(
      cursorKey,
      round.sessionId,
      continuationIdentity,
      continuationClientMessageHash,
    );
    if (continuationClientMessageId) {
      let completed;
      try {
        completed = readCompletedTurnReceipt(
          cursorKey,
          round.sessionId,
          continuationClientMessageId,
          continuationClientMessageHash,
        );
      } catch (error) {
        throw new ToolRoundError(
          "durable_turn_receipt_unavailable",
          (error && error.message) || String(error),
          503,
        );
      }
      if (completed) {
        if (prospectiveConflicts.length > 0) {
          throw new ToolRoundError(
            "result_conflict",
            `tool results ${prospectiveConflicts.join(", ")} conflict with the payload that produced the durable completed answer`,
            409,
          );
        }
        await replayCompletedTurn(res, completed);
        return;
      }
    }

    // A restart can occur after Node accepted the tool_call frame but before the
    // handoff timestamp reached the journal. A signed result from the same
    // authenticated tenant is proof the client saw that registered call, so a
    // non-live round may durably accept it and then recover/refuse faithfully.
    const committed = round.commitResults(results, {
      allowTerminalReceipt: !hasLiveCallbacks && wasTerminal,
      // Possession of a valid tenant-bound signed call id is stronger evidence
      // than the local handedAt timestamp: the process can crash or the SSE
      // writer can advance just before that metadata write. Accept it in both
      // warm and cold paths; the HMAC prevents an unseen id from being guessed.
      allowRegisteredReceipt: true,
      // Persistence is the exactly-once boundary even while sibling callbacks
      // remain live. A later stateless projection can be acknowledged but can
      // never replace the stored value or reach the SDK callback.
      preferDurableReceipt: true,
      // Result validation and optional user-intent journaling share one CAS
      // write. Invalid results cannot append poison entries or enlarge the
      // journal before returning an error.
      deferredInputId,
      deferredInput: hasDeferredInput ? input : null,
    });
    // Bind the final-answer receipt to the immutable result values that the
    // model actually receives. A lossy/conflicting stateless projection may
    // be tolerated before a completion exists, but ToolRound's first durable
    // receipts remain authoritative and therefore define the request hash.
    const authoritativeResults = results.map((result) => {
      const stored = round.calls.get(result && result.toolCallId)?.receipt?.result;
      return stored ? { toolCallId: result.toolCallId, ...stored } : result;
    });
    continuationClientMessageHash = completedTurnRequestHash({
      ...input,
      results: authoritativeResults,
    });
    if (committed.retainedConflicts.length > 0 && continuationClientMessageId) {
      const priorCompletion = readCompletedTurnReceipt(
        cursorKey,
        round.sessionId,
        continuationClientMessageId,
        continuationClientMessageHash,
      );
      if (priorCompletion) {
        throw new ToolRoundError(
          "result_conflict",
          `tool results ${committed.retainedConflicts.join(", ")} conflict with the payload that produced the durable completed answer`,
          409,
        );
      }
    }
    // A buggy client may reuse one clientMessageId for different immutable
    // intent. ToolRound preserves the original receipt and deterministically
    // rekeys the new intent; every subsequent delivery decision must use that
    // actual durable id rather than looking up the conflicting requested id.
    if (committed.deferredReceipt && committed.deferredReceipt.clientMessageId) {
      deferredInputId = committed.deferredReceipt.clientMessageId;
    }
    committed.inventoryWarning = inventoryWarning;
    if (historicalDispositions.length > 0) {
      committed.dispositions = [...historicalDispositions, ...committed.dispositions];
    }
    const committedIds = [...new Set([...committed.additions, ...committed.duplicates])];
    let deferred = deferredInputId ? round.deferredInput(deferredInputId) : null;
    if (!deferred) {
      // A client may stream a parallel result batch across multiple HTTP
      // requests. The request that carries the final sibling need not repeat
      // the trailing user intent from the first request; recover the one
      // actionable journal entry instead of silently dropping it.
      deferred = round.actionableDeferredInput();
      if (deferred) {
        deferredRound = round;
        deferredInputId = deferred.clientMessageId;
      }
    }
    const remainingUnreceipted = round.unreceiptedOwedCallCount;
    const liveOwner = hasLiveCallbacks ? sessions.get(round.sessionId) : null;
    const canHandRegisteredSibling = !!(liveOwner
      && liveOwner.currentRound === round
      && round.outbound.length > 0);

    // A retry of a pure tool-result continuation owns the same resumed model
    // turn even though it has no deferred user input. Hand the live response
    // writer to the retry before any terminal-round acknowledgement; otherwise
    // the actual answer keeps streaming to an abandoned socket while the retry
    // receives a misleading short success.
    const activeContinuationSession = sessions.get(round.sessionId);
    if (continuationClientMessageId
        && activeContinuationSession
        && activeContinuationSession.activeClientMessageId === continuationClientMessageId
        && activeContinuationSession.activeClientMessageHash === continuationClientMessageHash
        && (activeContinuationSession.run || activeContinuationSession.sendPending)
        && activeContinuationSession.lastSettledTurnToken < activeContinuationSession.turnToken) {
      await handoffActiveTurn(
        req,
        res,
        activeContinuationSession,
        round,
        committed,
        continuationClientMessageId,
      );
      return;
    }

    // A trailing user turn is a fresh-send boundary. Do not cross it until
    // every client-visible sibling call has a durable result receipt. The
    // callbacks intentionally remain paused; the final incremental request
    // rebuilds recovery from the complete journal, even when it contains only
    // the last sibling result.
    if (deferred
        && (deferred.state === DeferredInputState.QUEUED
          || deferred.state === DeferredInputState.DELIVERING
          || (deferred.state === DeferredInputState.DELIVERED && !deferred.deliveryFinalizedAt))
        && remainingUnreceipted > 0
        && !(canHandRegisteredSibling && !liveOwner.activeRes)) {
      // If an earlier response is still live, hand any newly registered tail
      // there before acknowledging this incremental result request.
      if (canHandRegisteredSibling && liveOwner.activeRes) liveOwner.flushJournaledCalls();
      await writeShortSse(res, {
        type: "turn_end",
        stop_reason: "tool_use",
        receipt: "partial_results_deferred_for_fidelity",
        outstandingToolCalls: remainingUnreceipted,
        ...continuationReceipt(round, committed, {
          clientMessageId: deferredInputId,
          userInputStatus: String(deferred.state || "").toLowerCase(),
        }),
      }, round, false);
      return;
    }

    if (deferred && deferred.state === DeferredInputState.SUPERSEDED) {
      const uncertain = typeof deferred.supersedeReason === "string"
        && deferred.supersedeReason.startsWith("delivery_uncertain");
      await writeShortSse(res, {
        type: "turn_end",
        stop_reason: uncertain ? "error" : "end_turn",
        receipt: uncertain ? "user_input_delivery_unknown" : "user_input_superseded",
        ...(uncertain ? {
          error: "the prior bridge process ended during SDK delivery; the durable checkpoint could not prove whether this input ran",
        } : {}),
        ...continuationReceipt(round, committed, {
          clientMessageId: deferredInputId,
          userInputStatus: "superseded",
        }),
      });
      return;
    }

    const activeDeferredSession = deferred ? sessions.get(round.sessionId) : null;
    if (deferred
        && (deferred.state === DeferredInputState.QUEUED
          || deferred.state === DeferredInputState.DELIVERING
          || deferred.state === DeferredInputState.DELIVERED)
        && activeDeferredSession
        && activeDeferredSession.activeDeferredInputId === deferredInputId
        && activeDeferredSession.activeRes
        && activeDeferredSession.lastSettledTurnToken < activeDeferredSession.turnToken) {
      await handoffActiveTurn(req, res, activeDeferredSession, round, committed, deferredInputId);
      return;
    }
    if (deferred
        && activeDeferredSession
        && activeDeferredSession.activeDeferredInputId === deferredInputId
        && activeDeferredSession.run
        && await replayPausedFreshTurn(res, activeDeferredSession, deferredInputId)) {
      return;
    }

    // `DELIVERING` is the deliberate crash boundary around agent.send. Text
    // found anywhere in agent history is not delivery evidence—a previous
    // identical "continue" is indistinguishable. Retry only the exact persisted
    // envelope, key, inventory epoch, and durable agent. The SDK patch maps
    // that key to one stable request id on that same agent.
    if (deferred && (deferred.state === DeferredInputState.DELIVERING
        || (deferred.state === DeferredInputState.DELIVERED && !deferred.deliveryFinalizedAt))) {
      uncertainDeliveryAgentId = typeof deferred.deliveryAgentId === "string"
        ? deferred.deliveryAgentId : "";
      const replayable = uncertainDeliveryAgentId
        && typeof deferred.deliveryIdempotencyKey === "string"
        && deferred.deliveryIdempotencyKey
        && Object.prototype.hasOwnProperty.call(deferred, "deliveryMessage")
        && Array.isArray(deferred.deliveryAdvertise);
      if (!replayable) {
        round.markDeferredInputState(deferredInputId, DeferredInputState.SUPERSEDED, {
          reason: "delivery_uncertain_missing_exact_envelope",
        });
        await writeShortSse(res, {
          type: "turn_end",
          stop_reason: "error",
          receipt: "user_input_delivery_unknown",
          retryable: false,
          error: "the prior bridge stopped during SDK delivery and the exact durable send envelope is unavailable; the proxy will not guess or duplicate it",
          ...continuationReceipt(round, committed, {
            clientMessageId: deferredInputId,
            userInputStatus: "superseded",
          }),
        });
        return;
      }
      if (deferred.deliveryInventoryEpoch
          && body.toolInventoryEpoch !== deferred.deliveryInventoryEpoch) {
        dbg("uncertain delivery retry carries a newer tool inventory; replaying the persisted send snapshot",
          "route=" + round.route, "original=" + deferred.deliveryInventoryEpoch,
          "current=" + body.toolInventoryEpoch);
      }
      round.requeueUncertainDeferredInput(deferredInputId);
      dbg("uncertain deferred delivery -> retry exact envelope on exact durable agent",
        "route=" + round.route, "clientMessageId=" + deferredInputId,
        "agentId=" + uncertainDeliveryAgentId);
    }
    if (deferred && deferred.state === DeferredInputState.DELIVERED && deferred.deliveryFinalizedAt) {
      let completed;
      try {
        completed = readCompletedTurnReceipt(
          cursorKey,
          round.sessionId,
          deferredInputId,
          continuationClientMessageHash,
        );
      } catch (error) {
        await writeShortSse(res, {
          type: "turn_end",
          stop_reason: "error",
          receipt: "durable_turn_receipt_unavailable",
          retryable: false,
          error: (error && error.message) || String(error),
          ...continuationReceipt(round, committed, {
            clientMessageId: deferredInputId,
            userInputStatus: "delivered",
          }),
        });
        return;
      }
      if (completed) {
        await replayCompletedTurn(res, completed);
        return;
      }
      await writeShortSse(res, {
        type: "turn_end",
        stop_reason: "error",
        receipt: "completed_response_receipt_unavailable",
        retryable: false,
        error: "the user input reached a final model response, but its durable answer receipt is no longer available; the proxy will not report an empty success or repeat side effects",
        ...continuationReceipt(round, committed, {
          clientMessageId: deferredInputId,
          userInputStatus: "delivered",
        }),
      }, round, true);
      return;
    }

    // A mixed tool-result + genuine user turn is two durable facts, not one
    // overloaded callback payload. Terminate the paused callback run, then
    // send an exact recovery summary plus the separately journaled user input
    // as a fresh SDK user message. On an in-process Session the durable agent
    // already owns the conversation, so bounded history is not required; on a
    // cold restart it is required before a new agent id may be seeded.
    if (deferred && deferred.state === DeferredInputState.QUEUED && remainingUnreceipted === 0) {
      const existing = sessions.get(round.sessionId);
      const completedReceipt = wasTerminal && (
        (round.terminal && round.terminal.reason === TerminalReason.COMPLETED)
        || (round.recovery && round.recovery.decision === "completed")
      );
      let session = existing || null;
      let freshInput;
      let isRecovery = !completedReceipt;
      const exactDeliveryRetry = !!uncertainDeliveryAgentId;

      if (session && session.currentRound && session.currentRound !== round) {
        if (session.currentRound.route !== round.route && !completedReceipt) {
          // A stateless client can replay an older receipted round while a
          // newer round is active. The signed source round still owns its
          // result; the newest user input is an interruption. Let the common
          // cancellation/recovery path below terminal-resolve the newer round
          // and continue from durable receipts instead of permanently 409ing.
          dbg("deferred source round differs from active round -> interrupt and recover",
            "session=" + session.id, "source=" + round.route, "active=" + session.currentRound.route);
        }
        if (session.currentRound.route === round.route) {
          // The live-map terminal callback removes completed rounds, so a later
          // request reloads the same route as a new object. Adopt that newer
          // journal revision instead of treating object identity as ownership.
          session.currentRound = round;
        }
      }
      if (session && keyFingerprint(session.cursorKey) !== keyFingerprint(cursorKey)) {
        throw new ToolRoundError("tenant_mismatch", "the active Session belongs to a different Cursor credential", 403);
      }

      const storedInput = deferredUserInput(round, deferred, input);
      if (completedReceipt) {
        freshInput = storedInput;
      } else if (session) {
        freshInput = buildResultRecoveryInput(round, storedInput);
      } else if (exactDeliveryRetry) {
        // The exact SDK envelope already contains the original seed/recovery
        // text. Do not require or rebuild a newer history snapshot.
        freshInput = storedInput;
      } else {
        freshInput = buildRestartRecoveryInput(round, storedInput);
        if (!freshInput) {
          round.recordRecovery({ at: nowMs(), decision: "refused", reason: "bounded conversation history is absent" });
          throw new ToolRoundError(
            "round_lost",
            "the paused SDK callback process is gone and this continuation has no bounded history for a faithful recovery",
            410,
          );
        }
      }

      if (!session) {
        if (!sessionCapHasRoomForNew()) throw new ToolRoundError("session_capacity", "session capacity is full", 429);
        session = new Session(round.sessionId, cursorKey);
        if (exactDeliveryRetry) {
          writeDurableAgentAlias(session.cursorKey, session.id, uncertainDeliveryAgentId, session);
          session.agentId = uncertainDeliveryAgentId;
        } else if (isRecovery) {
          const targetAgentId = recoveryTargetAgentId(round);
          session.agentId = targetAgentId;
          session.pendingRecoveryAlias = targetAgentId;
          session.resetSeedState();
        } else if (round.agentId) {
          writeDurableAgentAlias(session.cursorKey, session.id, round.agentId, session);
          session.agentId = round.agentId;
        }
        sessions.set(session.id, session);
      } else {
        if (exactDeliveryRetry && session.agentId !== uncertainDeliveryAgentId) {
          if (session.run || session.activeRes) {
            throw new ToolRoundError(
              "delivery_agent_busy",
              "the exact durable delivery agent is not available while another run owns this session",
              409,
            );
          }
          try { await (session.agent && session.agent.close && session.agent.close()); } catch {}
          session.agent = null;
          session.agentPromise = null;
          session.agentId = uncertainDeliveryAgentId;
          session.resetSeedState();
        }
        if (session.run || (session.currentRound && session.currentRound.state !== RoundState.TERMINAL)) {
          await session.cancel({ terminalReason: TerminalReason.INTERRUPTED, detail: "superseded by durable deferred user input" });
        }
      }

      session.clientLeaseToken = round.clientLeaseToken;
      refreshSessionFromBody(session, body, preparedToolRegistry, { ignoreToolInventory: !!inventoryWarning });
      if (isRecovery) {
        if (round.state !== RoundState.TERMINAL) {
          round.terminalize(TerminalReason.INTERRUPTED, "tool results and trailing user input redirected to a fresh SDK send");
        }
        round.recordRecovery({
          at: nowMs(),
          decision: exactDeliveryRetry
            ? "exact_delivery_retry"
            : existing ? "in_process_redirect" : "faithful_reseed",
          replacementAgentId: session.agentId,
          clientMessageId: deferredInputId,
          resultHashes: results.map((result) => round.calls.get(result.toolCallId).resultHash),
        });
        session.recoverySourceRound = round;
      }
      const constraints = {
        toolChoice: body.toolChoice || "",
        responseFormat: body.responseFormat,
        stop: body.stop,
        maxTokens: body.maxTokens,
        unsupportedHardGuarantees: body.unsupportedHardGuarantees,
      };
      return await enqueueTurn(req, res, session,
        (exactDeliveryRetry && deferred.deliveryModel) || body.model || round.model || "composer-2.5",
        freshInput, constraints, {
        round,
        committedIds,
        deferredInputId,
        deferredReceipt: committed.deferredReceipt,
        dispositions: committed.dispositions,
        requestHash: continuationClientMessageHash,
        identityPolicy: continuationIdentity.policy,
      });
    }

    if (hasLiveCallbacks) {
      const session = sessions.get(round.sessionId);
      if (!session || session.currentRound !== round) {
        try { round.terminalize(TerminalReason.RESTART_LOST, "live round lost its owning Session"); } catch {}
      } else {
        const completesLogicalToolRound = round.unreceiptedOwedCallCount === 0;
        // A tool inventory is a logical-turn contract, not mutable metadata on
        // each partial HTTP receipt. Applying partial snapshots lets delayed
        // continuations reorder schema updates or resurrect removed tools.
        // Freeze the registry until the final callback batch; that final
        // continuation defines the next model step's complete client surface.
        if (completesLogicalToolRound) {
          refreshSessionFromBody(session, body, preparedToolRegistry, { ignoreToolInventory: !!inventoryWarning });
        } else if (!inventoryWarning && body.toolInventoryEpoch !== session.toolInventoryEpoch) {
          dbg("deferred tool inventory refresh until logical ToolRound boundary",
            "session=" + session.id, "current=" + session.toolInventoryEpoch, "submitted=" + body.toolInventoryEpoch);
        }
        const constraints = {
          toolChoice: body.toolChoice || "",
          responseFormat: body.responseFormat,
          stop: body.stop,
          maxTokens: body.maxTokens,
          unsupportedHardGuarantees: body.unsupportedHardGuarantees,
        };
        const recoveryPlan = toolResultRecoveryPlan(round, input, session);
        if (session.activeRes && recoveryPlan.requiresFreshRecovery
            && recoveryPlan.remainingUnreceipted > 0) {
          await writeShortSse(res, {
            type: "turn_end",
            stop_reason: "tool_use",
            receipt: "partial_results_deferred_for_fidelity",
            outstandingToolCalls: recoveryPlan.remainingUnreceipted,
            ...continuationReceipt(round, committed),
          }, round, false);
          return;
        }
        if (session.activeRes && recoveryPlan.requiresFreshRecovery) {
          // A concurrent continuation may still own the previous response
          // while this request supplies the final image/system/user payload.
          // End that paused run truthfully, reject every callback exactly
          // once, then let this request own the faithful journal recovery.
          session.sse({
            type: "turn_end",
            stop_reason: "error",
            error: "turn superseded by a complete client-tool recovery batch",
            _clientLeaseTerminal: false,
          });
          await session.cancel({
            terminalReason: TerminalReason.INTERRUPTED,
            detail: "superseded by a complete client-tool recovery batch",
          });
          session.done = false;
          session.currentRound = round;
        } else if (session.activeRes) {
          round.applyCommittedResults(committedIds);
          await writeShortSse(res, {
            type: "turn_end",
            stop_reason: round.pendingCount > 0 ? "tool_use" : "end_turn",
            receipt: "duplicate_or_concurrent",
            ...continuationReceipt(round, committed),
          });
          return;
        }
        res.writeHead(200, SSE_HEADERS);
        await runTurn(req, res, session, body.model || round.model || "composer-2.5", input, constraints, {
          round,
          committedIds,
          requestHash: continuationClientMessageHash,
          identityPolicy: continuationIdentity.policy,
        });
        return;
      }
    }

    // ToolRound COMPLETED means only that every paused SDK callback received
    // its client result. It does not prove that the resumed model turn reached
    // a final answer. A completed-turn receipt was checked at ingress; absent
    // that receipt, continue through faithful recovery instead of returning a
    // fake `already_applied` success.
    if (wasTerminal && round.recovery && round.recovery.decision === "completed" && committed.additions.length === 0) {
      await writeShortSse(res, {
        type: "turn_end",
        stop_reason: "error",
        receipt: "completed_response_receipt_unavailable",
        retryable: false,
        error: "the recovery run reached a final response, but no durable answer receipt is available; the proxy will not report an empty success or repeat side effects",
        ...continuationReceipt(round, committed),
      }, round, true);
      return;
    }

    const recoveryInput = buildRestartRecoveryInput(round, input);
    if (round.state !== RoundState.TERMINAL) round.terminalize(TerminalReason.RESTART_LOST, "paused SDK callback process is no longer present");
    if (!recoveryInput) {
      round.recordRecovery({ at: nowMs(), decision: "refused", reason: "bounded conversation history is absent" });
      throw new ToolRoundError(
        "round_lost",
        "the paused SDK callback process is gone and this continuation has no bounded history for a faithful recovery",
        410,
      );
    }
    let session = sessions.get(round.sessionId) || null;
    if (session && keyFingerprint(session.cursorKey) !== keyFingerprint(cursorKey)) {
      throw new ToolRoundError("tenant_mismatch", "the active Session belongs to a different Cursor credential", 403);
    }
    if (session) {
      // A stale in-memory Session is not a routing conflict. Terminal-resolve
      // whatever it still owns and reuse the stable conversation slot for the
      // durable source round. Returning 409 here made every retry deterministic
      // failure until process restart.
      if (session.activeRes) {
        session.sse({
          type: "turn_end",
          stop_reason: "error",
          error: "turn interrupted by durable client-tool recovery",
        });
      }
      await session.cancel({
        terminalReason: TerminalReason.INTERRUPTED,
        detail: "superseded by durable client-tool recovery",
      });
      session.done = false;
      session.model = null;
      session.currentRound = null;
    } else {
      if (!sessionCapHasRoomForNew()) throw new ToolRoundError("session_capacity", "session capacity is full", 429);
      session = new Session(round.sessionId, cursorKey);
      sessions.set(session.id, session);
    }
    session.clientLeaseToken = round.clientLeaseToken;
    const recoveryAgentId = recoveryTargetAgentId(round);
    session.agentId = recoveryAgentId;
    session.pendingRecoveryAlias = recoveryAgentId;
    session.recoverySourceRound = round;
    session.resetSeedState();
    refreshSessionFromBody(session, body, preparedToolRegistry, { ignoreToolInventory: !!inventoryWarning });
    round.recordRecovery({
      at: nowMs(),
      decision: "faithful_reseed",
      replacementAgentId: session.agentId,
      resultHashes: results.map((result) => round.calls.get(result.toolCallId).resultHash),
    });
    const constraints = {
      toolChoice: body.toolChoice || "",
      responseFormat: body.responseFormat,
      stop: body.stop,
      maxTokens: body.maxTokens,
      unsupportedHardGuarantees: body.unsupportedHardGuarantees,
    };
    res.writeHead(200, SSE_HEADERS);
    await runTurn(req, res, session, body.model || round.model || "composer-2.5", recoveryInput, constraints, {
      round,
      committedIds,
      dispositions: committed.dispositions,
      requestHash: continuationClientMessageHash,
      identityPolicy: continuationIdentity.policy,
    });
  } catch (error) {
    if (res.headersSent) {
      dbg("continuation failed after SSE headers; emitting typed terminal",
        (error && error.stack) || (error && error.message) || String(error));
      await finishStartedSseWithError(res, {
        receipt: "continuation_failure_after_headers",
        retryable: true,
        retryMode: "identical",
        error: "the continuation failed after its response stream started; retry the identical continuation",
      });
      return;
    }
    if (error instanceof ToolRoundError) {
      if (error.code === "journal_revision_conflict" && casAttempt < 3) {
        if (routedRound && liveToolRounds.get(routedRound.route) === routedRound) {
          liveToolRounds.delete(routedRound.route);
          const staleSession = sessions.get(routedRound.sessionId);
          if (staleSession && staleSession.currentRound === routedRound) {
            const fenced = new Error("durable ToolRound ownership advanced in another bridge process");
            for (const call of routedRound.calls.values()) {
              routedRound.clearTimer(call);
              for (const callback of [call.callback, ...(call.waiters || [])].filter(Boolean)) {
                try { callback.reject(fenced); } catch {}
              }
              call.callback = null;
              call.waiters = [];
            }
            routedRound.callbacks.clear();
            staleSession.currentRound = null;
            staleSession.lastRunError = fenced.message;
            sessions.delete(staleSession.id);
            cancelSessionDetached(staleSession, {
              terminalReason: TerminalReason.RUN_ERROR,
              detail: fenced.message,
            });
          }
        }
        dbg("continuation CAS changed concurrently -> reload and retry",
          "attempt=" + (casAttempt + 1), (error && error.message) || String(error));
        return handleContinue(req, res, body, cursorKey, casAttempt + 1, historicalDispositions);
      }
      // A continuation conflict must never become a transport-level 409 loop.
      // Known replay conflicts are resolved above; this is the fail-safe for a
      // residual state-machine race. Return a normal SSE terminal without
      // consuming any additional client intent, preserving the durable round
      // for an independent retry and keeping generic harnesses operational.
      if (error.status === 409) {
        const replayIdentical = error.code === "journal_revision_conflict";
        dbg("contained residual continuation conflict as typed SSE receipt",
          "code=" + error.code, (error && error.message) || String(error));
        await writeShortSse(res, {
          type: "turn_end",
          stop_reason: "error",
          receipt: "continuation_conflict_contained",
          retryable: replayIdentical,
          retryMode: replayIdentical ? "identical" : "repair",
          errorCode: error.code,
          error: error.message,
          ...(routedRound ? { toolEpochState: routedRound.toolEpochState() } : {}),
        });
        return;
      }
      continuationFailure(
        res,
        error.code === "journal_revision_conflict" ? 503 : error.status,
        error.code,
        error.message,
        deferredErrorDetails(deferredRound, deferredInputId, error.details),
      );
      return;
    }
    continuationFailure(res, 500, "continue_failed", (error && error.message) || String(error));
  }
}

async function handleTurn(req, res, body, cursorKey) {
  if (!body || typeof body !== "object" || Array.isArray(body)) {
    res.writeHead(400, { "Content-Type": "application/json" });
    res.end(JSON.stringify({ error: "request body must be a JSON object" }));
    return;
  }
  const input = body.input || (body.text != null ? { type: "user", text: body.text } : { type: "user", text: "" });
  const model = body.model || "composer-2.5";
  const sessionId = body.sessionId;
  const fail = (code, msg, errorCode = "") => {
    dbg("handleTurn FAIL", code, "session=" + sessionId, "inputType=" + (input && input.type), msg);
    res.writeHead(code, { "Content-Type": "application/json" });
    res.end(JSON.stringify({ error: errorCode ? { code: errorCode, message: msg } : msg }));
  };
  // Validate BEFORE opening the SSE so we can return a real HTTP status.
  if (!sessionId) { fail(400, "sessionId is required"); return; }
  let requestIdentity;
  let requestClientMessageHash;
  try {
    requestIdentity = turnInvocationIdentity(input);
    requestClientMessageHash = completedTurnRequestHash(input);
  } catch (error) {
    fail(error instanceof ToolRoundError ? error.status : 422, (error && error.message) || String(error));
    return;
  }
  const requestClientMessageId = requestIdentity.id;
  let freshDeliveryGeneration = 1;
  let freshDeliveryReceipt = null;
  if (requestClientMessageId) {
    let completed = null;
    try {
      if (input.type === "user") {
        freshDeliveryReceipt = readFreshDeliveryReceipt(
          cursorKey,
          sessionId,
          requestClientMessageId,
          requestClientMessageHash,
        );
      }
      completed = freshDeliveryReceipt ? null
        : readCompletedTurnReceipt(cursorKey, sessionId, requestClientMessageId, requestClientMessageHash);
    } catch (error) {
      fail(503, `durable turn continuity state is unavailable: ${(error && error.message) || String(error)}`);
      return;
    }
    if (freshDeliveryReceipt && unresolvedFreshDeliveryReceipt(freshDeliveryReceipt)) {
      freshDeliveryGeneration = freshDeliveryReceipt.generation;
    } else if (completed) {
      const provenLegacyRecovery = input.type === "user"
        && typeof input.legacyRecoveryKey === "string"
        && input.legacyRecoveryKey === legacyRecoveryKeyFor(
          sessionId,
          typeof input.clientMessageId === "string" ? input.clientMessageId.trim() : requestClientMessageId,
        );
      if (input.type !== "user" || provenLegacyRecovery || versionedFreshReplayIdentity(requestIdentity)) {
        await replayCompletedTurn(res, completed);
        return;
      }
      // Explicit legacy-client-message-v1 compatibility: an arbitrary old
      // fresh ID did not define whether reuse meant retry or intentional
      // repeat. Keep the historical execute-again behavior only for those
      // unversioned IDs. invocation-v1 and ccm2 identities are authoritative
      // logical calls and always replay an identical completed request.
      freshDeliveryGeneration = (Number.isSafeInteger(completed.generation)
        ? completed.generation : 1) + 1;
    }
  }
  if (input.type === "user") {
    input.deliveryGeneration = freshDeliveryGeneration;
    delete input.freshDeliveryReceipt;
    if (freshDeliveryReceipt) input.freshDeliveryReceipt = freshDeliveryReceipt;
  }
  const legacyRecoveryKey = typeof input.legacyRecoveryKey === "string" ? input.legacyRecoveryKey.trim() : "";
  if (legacyRecoveryKey) {
    const clientMessageId = typeof input.clientMessageId === "string" ? input.clientMessageId.trim() : "";
    if (input.type !== "user" || !clientMessageId
        || legacyRecoveryKey !== legacyRecoveryKeyFor(sessionId, clientMessageId)) {
      fail(422, "legacy recovery identity is malformed");
      return;
    }
  }
  if (!validClientEnv(body.clientEnv)) body.clientEnv = { workspaceUnknown: true };
  let preparedToolRegistry;
  let inventoryWarning = "";
  try {
    preparedToolRegistry = prepareAdvertisedToolRegistry(body);
  } catch (error) {
    inventoryWarning = (error && error.message) || String(error);
    // A malformed dynamic replacement must not kill an established
    // conversation. Keep the last known-good registry and let the harness
    // correct its next snapshot. A brand-new session has no safe tool contract
    // to retain, so it still receives the explicit configuration error.
    if (!sessions.has(sessionId)) {
      fail(422, inventoryWarning);
      return;
    }
    preparedToolRegistry = undefined;
    dbg("new-turn replacement inventory rejected; preserving prior registry",
      "session=" + sessionId, inventoryWarning);
  }

  // Upstream rate-limit circuit breaker: while OPEN for this key, fast-fail NEW runs with a clear 429 so client
  // retries back off instead of re-poisoning the freshly-recycled HTTP/2 connection. tool_results continuations
  // are NOT gated — they complete a paused run, and blocking one would strand it until the abandonment watchdog.
  // HALF-OPEN after the window: the next new-user turn probes and closeBreaker (onRunComplete) clears it on success.
  if (input.type !== "tool_results") {
    const kh = keyHash(cursorKey);
    if (breakerOpen(kh)) {
      const waitS = Math.ceil(breakerRetryAfterMs(kh) / 1000);
      fail(429, `upstream is rate-limiting this account (Cursor HTTP/2 ENHANCE_YOUR_CALM); the proxy recycled the connection and is backing off — retry in ~${waitS}s and avoid rapid retries (they re-trip the limit)`, "upstream_account_rate_limit");
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
    fail(400, "tool results must be submitted to /agent/continue using their signed tool-call ids");
    return;
  }

  let session = sessions.get(sessionId);
  if (!session) {
    if (!sessionCapHasRoomForNew()) { fail(429, `session capacity reached (${MAX_SESSIONS}); all sessions are active or paused — retry later`, "session_capacity"); return; }
    if (MULTI_TENANT && !platformCapHasRoomForNew(cursorKey)) { fail(429, `platform capacity reached (${MAX_PLATFORMS}); all tenant platforms are in use — retry later`, "platform_capacity"); return; }
  }

  let preReservedReceiptFile = "";
  const releasePreReservedReceipt = () => {
    if (!preReservedReceiptFile) return;
    try { releaseUnresolvedReceipt(preReservedReceiptFile); } catch {}
    preReservedReceiptFile = "";
    delete input.preReservedReceiptFile;
  };
  // Exact retries with an existing UNKNOWN/RUNNING receipt can recover without
  // allocating another large envelope. A genuinely fresh send reserves shared
  // durable capacity only after every request-shape/admission preflight above,
  // but before invocation binding, session mutation, or SDK work.
  if (input.type === "user" && !freshDeliveryReceipt
      && !stateRootDiskStatus(MAX_AGENT_TURN_BYTES * 2).ok) {
    fail(507,
      "durable state storage does not have enough free capacity to accept a new recoverable turn",
      "durable_state_capacity");
    return;
  }
  if (input.type === "user" && requestClientMessageId && !freshDeliveryReceipt) {
    preReservedReceiptFile = completedTurnReceiptPath(
      cursorKey, sessionId, requestClientMessageId, requestClientMessageHash,
    );
    try {
      reserveUnresolvedReceipt(preReservedReceiptFile, MAX_AGENT_TURN_BYTES * 2);
    } catch (error) {
      fail(error instanceof ToolRoundError ? error.status : 507,
        (error && error.message) || String(error),
        "durable_state_capacity");
      return;
    }
    input.preReservedReceiptFile = preReservedReceiptFile;
  }
  try {
    bindInvocationRequestHash(
      cursorKey,
      sessionId,
      requestIdentity,
      requestClientMessageHash,
    );
  } catch (error) {
    releasePreReservedReceipt();
    fail(error instanceof ToolRoundError ? error.status : 503,
      (error && error.message) || String(error),
      error instanceof ToolRoundError ? error.code : "durable_invocation_binding_unavailable");
    return;
  }

  // New-user turn: get-or-create the session, refresh advertised tools/env, then enqueue on the per-session
  // FIFO. The chain serializes concurrent new-user turns (idle -> runs immediately; busy -> waits, kept
  // alive) instead of 409-rejecting them, so no client ever sees a retryable error from a collision.
  if (!session) {
    // ADD-63 (LOAD-SHED): reject a NEW session when at cap and nothing idle can be shed. Never evict a live or
    // paused session to admit this one. ADD-75: likewise reject a NEW tenant (new platform) at the platform cap.
    try {
      session = new Session(sessionId, cursorKey);
    } catch (error) {
      releasePreReservedReceipt();
      fail(503, `durable session continuity state is unavailable: ${(error && error.message) || String(error)}`);
      return;
    }
    sessions.set(sessionId, session); dbg("handleTurn NEW session", sessionId);
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
      try { await session.rotateForKeyChange(cursorKey); }
      catch (error) { releasePreReservedReceipt(); throw error; }
    }
  }
  // Exact active retries retain their original generation and SDK envelope.
  // Legacy FAILED receipts remain acceptance-unknown and reach this same
  // reattachment path; they never authorize an unproven second generation.
  const incomingClientMessageId = requestClientMessageId;
  const incomingClientMessageHash = requestClientMessageHash;
  if (incomingClientMessageId
      && session.activeClientMessageId === incomingClientMessageId
      && session.activeClientMessageHash === incomingClientMessageHash
      && (session.run || session.sendPending)) {
    if (session.activeRes && session.lastSettledTurnToken < session.turnToken) {
      await handoffActiveTurn(req, res, session, null, null, incomingClientMessageId);
      return;
    }
    if (await replayPausedFreshTurn(res, session, incomingClientMessageId)) return;
    // No response and no handed tool call means the prior transport vanished
    // before a replayable boundary. Fall through to the ordinary interrupt +
    // resend path using the same SDK idempotency key.
  }
  if (legacyRecoveryKey) {
    try {
      await session.rotateForLegacyRecovery(legacyRecoveryKey);
    } catch (error) {
      releasePreReservedReceipt();
      fail(503, `cannot establish durable legacy recovery: ${(error && error.message) || String(error)}`);
      return;
    }
  }
  // A new logical user turn replaces the prior lease token. Exact active
  // retries returned above keep the token already attached to that run.
  session.clientLeaseToken = normalizeClientLeaseToken(body.clientLeaseToken);
  try {
    refreshSessionFromBody(session, body, preparedToolRegistry, { ignoreToolInventory: !!inventoryWarning });
  } catch (error) {
    releasePreReservedReceipt();
    throw error;
  }
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
  // A genuine user interruption is always cancel-and-replace. Tool results and mixed trailing user input arrive
  // through /agent/continue, where each is journaled independently; this /agent/turn path never mutates or rides
  // a user instruction on a tool callback. Resolving old pendings and starting a new send concurrently would let
  // the superseded run collide with its successor on the same durable agent.
  // The interrupted (unanswered) tool-call INTENT is NOT lost to the redirected turn: cancel() keeps session.agentId
  // + session.seeded, so the fresh send's ensureAgent resumeAgent()s the DURABLE Cursor agent, which holds the prior
  // assistant tool-call turn server-side (ensureAgent: a successful resume implies prior turns -> seeded, no
  // re-prepend). And the dropped pendings are NOT a false success: cancel() rejectAllPending("session cancelled")
  // fails each awaiting tool promise model-visibly — the new turn's terminal is the only clean end_turn/[DONE], and
  // it belongs to the new instruction, never to the superseded tool calls.
  if (session.run) {
    dbg("handleTurn USER INTERRUPT of a live run -> cancel stale run + drive new turn", sessionId, "streaming=" + !!session.activeRes, "pending=" + session.pendingCount(), "waiters=" + session.waiters);
    try { await session.cancel({ terminalReason: TerminalReason.INTERRUPTED, detail: "superseded by a new user turn" }); }
    catch (error) { releasePreReservedReceipt(); throw error; }
  }
  if (freshDeliveryReceipt && session.agentId !== freshDeliveryReceipt.agentId) {
    try { await (session.agent && session.agent.close && session.agent.close()); } catch {}
    session.agent = null;
    session.agentPromise = null;
    session.agentId = freshDeliveryReceipt.agentId;
    session.resetSeedState();
  }
  if (session.waiters >= MAX_QUEUE_DEPTH) {
    releasePreReservedReceipt();
    fail(429, "too many concurrent turns queued for this session", "session_queue_capacity");
    return;
  }
  return enqueueTurn(
    req,
    res,
    session,
    (freshDeliveryReceipt && freshDeliveryReceipt.deliveryModel) || model,
    input,
    constraints,
  );
}

// enqueueTurn serializes a new-user turn on the session's FIFO chain. It opens the SSE + a client-facing
// keepalive IMMEDIATELY (so a queued turn looks like one slow-but-live stream, never a silent/failed one),
// waits EVENT-DRIVEN for the prior turn's run to truly complete (session.tail + whenLogicalDone — no
// wall-clock timer), then runs in-order on the same session. A queued waiter's disconnect removes ONLY that
// waiter; it never tears down the session (which would kill the active turn + other waiters).
function enqueueTurn(req, res, session, model, input, constraints, continuation = null) {
  session.waiters++;
  session.touch();
  res.writeHead(200, SSE_HEADERS);
  let queueBlocked = false;
  const onDrainQueue = () => { queueBlocked = false; };
  if (typeof res.on === "function") res.on("drain", onDrainQueue);
  const ka = setInterval(() => {
    if (queueBlocked) return;
    try {
      const ok = res.write(formatSseData({ type: "ping" }));
      if (ok === false) queueBlocked = true;
    } catch {}
  }, SSE_KEEPALIVE_MS);
  if (ka.unref) ka.unref();
  let canceled = false;
  let reaped = false;
  const onWaitClose = () => {
    if (reaped) return;
    reaped = true; canceled = true;
    clearInterval(ka);
    res.off("close", onWaitClose);
    if (typeof res.off === "function") res.off("drain", onDrainQueue);
    session.waiters = Math.max(0, session.waiters - 1);
    try { res.end(); } catch {}
  };
  res.on("close", onWaitClose);

  const prev = session.tail;
  let releaseNext;
  session.tail = new Promise((r) => { releaseNext = r; });

  prev.then(async () => {
    if (reaped) return;
    clearInterval(ka);
    res.off("close", onWaitClose);
    if (typeof res.off === "function") res.off("drain", onDrainQueue);
    session.waiters = Math.max(0, session.waiters - 1);
    if (canceled) { try { res.end(); } catch {} return; }
    try {
      await runTurn(req, res, session, model, input, constraints, continuation);
      await session.whenLogicalDone();
    } catch (e) { dbg("enqueueTurn run error", "session=" + session.id, (e && e.message) || String(e)); }
  }).finally(() => { releaseNext(); });
}

function prepareAdvertisedToolRegistry(body) {
  let tools = body && body.tools;
  if (body && Object.prototype.hasOwnProperty.call(body, "contractVersion")) {
    if (body.contractVersion !== CLIENT_TOOL_CONTRACT_VERSION) {
      throw new Error(`unsupported client-tool contractVersion ${String(body.contractVersion)}`);
    }
    if (body.toolsAuthoritative !== true) {
      throw new Error("client-tool contract v2 requires toolsAuthoritative:true");
    }
    if (typeof body.toolInventoryEpoch !== "string" || !/^cti1_[0-9a-f]{32}$/.test(body.toolInventoryEpoch)) {
      throw new Error("client-tool contract v2 requires a valid toolInventoryEpoch");
    }
    if (typeof body.toolInventoryJSON !== "string") {
      throw new Error("client-tool contract v2 requires an authoritative toolInventoryJSON snapshot");
    }
    if (Object.prototype.hasOwnProperty.call(body, "tools")) {
      throw new Error("client-tool contract v2 accepts exactly one authoritative tool inventory representation");
    }
    if (Buffer.byteLength(body.toolInventoryJSON, "utf8") > MAX_AGENT_TURN_BYTES) {
      throw new Error("client-tool contract v2 toolInventoryJSON exceeds the request byte limit");
    }
    const expectedEpoch = "cti1_" + createHash("sha256")
      .update(body.toolInventoryJSON, "utf8")
      .digest("hex")
      .slice(0, 32);
    if (body.toolInventoryEpoch !== expectedEpoch) {
      throw new Error("toolInventoryEpoch does not match the authoritative tools snapshot");
    }
    try {
      tools = JSON.parse(body.toolInventoryJSON);
    } catch {
      throw new Error("client-tool contract v2 toolInventoryJSON is not valid JSON");
    }
    if (!Array.isArray(tools)) {
      throw new Error("client-tool contract v2 toolInventoryJSON must encode a tools array");
    }
    // From this point onward every bridge consumer reads the one verified
    // snapshot. Do not mutate body by materializing a second `tools` field:
    // preparation must be idempotent and there must remain exactly one wire
    // representation whose key order, number formatting, or escaping can be
    // checked.
  }
  if (!Array.isArray(tools)) return undefined;
  const registry = new AdvertisedToolRegistry();
  const normalized = [];
  const seenNames = new Set();
  const permissiveSchema = Object.freeze({ type: "object", additionalProperties: true });
  let rejectedCount = 0;
  for (const rawTool of tools) {
    if (!rawTool || typeof rawTool !== "object" || Array.isArray(rawTool)
        || typeof rawTool.name !== "string" || !rawTool.name.trim()) {
      dbg("skipping an unrouteable advertised tool descriptor without a string name");
      rejectedCount++;
      continue;
    }
    const name = rawTool.name.trim();
    if (seenNames.has(name)) {
      // OpenAI/Anthropic tool results route by name, so two descriptors with
      // one name are not independently addressable. Retaining the first stable
      // descriptor keeps every other client tool functional without rejecting
      // the entire turn for one malformed inventory entry.
      dbg("ignoring duplicate advertised tool descriptor", "name=" + name);
      rejectedCount++;
      continue;
    }
    seenNames.add(name);
    const aliases = Array.isArray(rawTool.aliases)
      ? [...new Set(rawTool.aliases
        .filter((alias) => typeof alias === "string" && alias.trim() && alias.length <= 256)
        .slice(0, 64)
        .map((alias) => alias.trim()))]
      : [];
    if (rawTool.aliases !== undefined && aliases.length === 0) {
      dbg("ignoring malformed private tool aliases", "name=" + name);
    }
    const suppliedSchema = Object.hasOwn(rawTool, "inputSchema")
      ? rawTool.inputSchema
      : (Object.hasOwn(rawTool, "parameters") ? rawTool.parameters : undefined);
    // JSON Schema `true` really is an unconstrained schema. `false`, a
    // primitive, or an invalid schema is not. Widening those failures to a
    // permissive object made Cursor omit required arguments and pushed the
    // resulting validation error into the harness. Quarantine only the bad
    // descriptor so every independent valid client tool remains available.
    if (suppliedSchema === false
        || (suppliedSchema !== undefined && suppliedSchema !== true
          && (!suppliedSchema || typeof suppliedSchema !== "object" || Array.isArray(suppliedSchema)))) {
      rejectedCount++;
      dbg("quarantining advertised tool with a non-object input schema", "name=" + name);
      continue;
    }
    const inputSchema = suppliedSchema === true
      ? permissiveSchema
      : augmentUnderspecifiedToolSchema(name, suppliedSchema);
    const descriptor = {
      name,
      toolName: name,
      providerIdentifier: CLIENT_TOOL_PROVIDER_ID,
      description: typeof rawTool.description === "string" ? rawTool.description : "",
      inputSchema,
      ...(aliases.length ? { aliases } : {}),
    };
    try {
      const probe = new AdvertisedToolRegistry();
      probe.replace([descriptor]);
    } catch (error) {
      rejectedCount++;
      dbg("quarantining advertised tool with an invalid input schema", "name=" + name,
        (error && error.message) || String(error));
      continue;
    }
    normalized.push(descriptor);
  }
  registry.replace(normalized);
  registry.rejectedCount = rejectedCount;
  return registry;
}

function refreshSessionFromBody(session, body, preparedToolRegistry = undefined, { ignoreToolInventory = false } = {}) {
  session.utilityOneShot = body && body.utilityOneShot === true;
  const hasAuthoritativeSnapshot = body && body.contractVersion === CLIENT_TOOL_CONTRACT_VERSION
    && typeof body.toolInventoryJSON === "string";
  if (!ignoreToolInventory && (Array.isArray(body && body.tools) || hasAuthoritativeSnapshot)) {
    const candidate = preparedToolRegistry || prepareAdvertisedToolRegistry(body);
    const preserveLastKnownGood = candidate
      && candidate.rejectedCount > 0
      && candidate.all().length === 0
      && session.toolRegistry.all().length > 0;
    if (preserveLastKnownGood) {
      dbg("all replacement tool descriptors were quarantined; preserving the last-known-good registry",
        "session=" + session.id, "rejected=" + candidate.rejectedCount);
    } else {
      session.toolRegistry = candidate;
      session.toolInventoryEpoch = typeof body.toolInventoryEpoch === "string" ? body.toolInventoryEpoch : null;
    }
    session.toolChoice = typeof body.toolChoice === "string" ? body.toolChoice : "";
    if (session.activeAdvertise !== null) {
      session.activeAdvertise = effectiveAdvertise(session.advertise, session.toolChoice);
    }
  }
  if (body.clientEnv && typeof body.clientEnv === "object") {
    session.clientEnv = body.clientEnv;
  }
}

async function runTurn(req, res, session, model, input, constraints = {}, continuation = null) {
  // ADD-98/ADD-101: declare the turn-latch + close handler BEFORE the first res.write and before assigning
  // session.activeRes, then register res.on('close') first and wrap the whole body (from the activeRes
  // assignment onward) in try/finally. A throw on the INITIAL write (a socket already destroyed before/at the
  // first write) must not strand session.activeRes set or leak the keepalive — the finally always clears them.
  let settled = false;
  let resolveTurn;
  const turnSettled = new Promise((resolve) => { resolveTurn = resolve; });
  const settleOnce = () => { if (!settled) { settled = true; resolveTurn(); } };
  const deferredRound = continuation && continuation.round;
  const deferredInputId = continuation && continuation.deferredInputId;
  const requestIdentity = turnInvocationIdentity(input);
  const requestedClientMessageId = requestIdentity.id;
  const clientMessageId = deferredInputId || requestedClientMessageId;
  let identityPolicy = continuation?.identityPolicy || requestIdentity.policy;
  if (identityPolicy === TURN_IDENTITY_POLICY.NONE && clientMessageId) {
    // A later sibling-only continuation can recover an earlier journaled
    // invocation without repeating its identity field. The durable deferred
    // id still owns the send; legacy policy here records the compatibility
    // provenance without changing that identity or its SDK key.
    identityPolicy = TURN_IDENTITY_POLICY.LEGACY_CLIENT_MESSAGE_V1;
  }
  const clientMessageHash = continuation
      && typeof continuation.requestHash === "string"
      && /^[a-f0-9]{64}$/.test(continuation.requestHash)
    ? continuation.requestHash
    : completedTurnRequestHash(input);
  const clientMessageGeneration = Number.isSafeInteger(input?.deliveryGeneration)
    && input.deliveryGeneration >= 1 ? input.deliveryGeneration : 1;
  const clientMessageKind = continuation ? "continuation" : "fresh";
  const markDeferred = (state, metadata = null) => {
    if (!deferredRound || !deferredInputId) return;
    deferredRound.markDeferredInputState(deferredInputId, state, metadata);
  };
  // Install retry ownership before the first await. A duplicate HTTP request
  // arriving while ensureAgent/agent.send is still starting can attach to this
  // response instead of queuing a second send for the same client message.
  if (clientMessageId) {
    session.activeClientMessageId = clientMessageId;
    session.activeClientMessageHash = clientMessageHash;
    session.activeClientMessageGeneration = clientMessageGeneration;
    session.activeClientMessageKind = clientMessageKind;
    session.activeIdentityPolicy = identityPolicy;
    session.sendPending = true;
  }
  if (deferredInputId) {
    session.activeDeferredInputId = deferredInputId;
  }
  // If the client/proxy disconnects MID-turn, settle this turn (so the finally runs and keepalive clears)
  // and cancel the live run. cancel() fires notifyLogicalDone(), advancing the FIFO to the next waiter. A
  // close that arrives AFTER the turn already settled is a normal end-of-turn socket close and must NOT
  // cancel the paused run the next tool_results turn needs. Only DELETE the session when no waiters remain —
  // otherwise a queued turn on the same conversation would be stranded by the active turn's disconnect.
  const onClose = () => {
    if (settled) return;
    // A same-message retry may have atomically handed this live turn to a new
    // response sink. The superseded socket no longer owns cancellation.
    if (session.activeRes !== res) return;
    dbg("response.close -> cancel",
      "session=" + session.id,
      "turnToken=" + session.turnToken,
      "run=" + !!session.run,
      "pending=" + session.pendingCount(),
      "waiters=" + session.waiters);
    settleOnce();
    // Reject receipts before clearing queue on close (transport failure path)
    session.failWrite("response closed");
    if (!session.hasQueuedWaiters()) sessions.delete(session.id);
  };
  let keepalive = null;
  try {
    try {
      session.beginReplaySegment(clientMessageId);
    } catch (error) {
      if (!error || error.code !== "local_replay_capacity") throw error;
      if (res.headersSent) {
        session.beginResponse(res);
        session.sse({
          type: "turn_end",
          stop_reason: "error",
          errorCode: error.code,
          error: error.message,
          retryable: true,
        });
      } else {
        res.writeHead(error.status || 503, { "Content-Type": "application/json", "Cache-Control": "no-store" });
        res.end(JSON.stringify({ error: { code: error.code, message: error.message } }));
      }
      if (typeof input.preReservedReceiptFile === "string" && input.preReservedReceiptFile) {
        try { releaseUnresolvedReceipt(input.preReservedReceiptFile); } catch {}
        delete input.preReservedReceiptFile;
      }
      settleOnce();
      session.notifyLogicalDone();
      if (!session.seeded && session.agent === null && session.run === null
          && session.pendingCount() === 0 && !session.hasQueuedWaiters()
          && sessions.get(session.id) === session) sessions.delete(session.id);
      return;
    }
    session.beginResponse(res); session.touch(); session.turnToken++;
    {
      const caps = input && (input.capabilities || input.clientCapabilities || "");
      const wantsResume = input?.streamResume === true || hasStreamResumeCapability(caps);
      session.streamResumeEnabled = wantsResume === true;
      session.invocationId = (requestIdentity && requestIdentity.id) || clientMessageId || "";
      if (session.streamResumeEnabled && !session.streamJournal) {
        throw new ToolRoundError(
          "stream_journal_unavailable",
          "stream-resume-v1 requires a durable state store; set CLIPROXY_STATE_SOCKET",
          503,
        );
      }
      if (session.streamResumeEnabled && !session.invocationId) {
        throw new ToolRoundError(
          "stream_resume_invocation_required",
          "stream-resume-v1 requires an invocation identity before live expose",
          422,
        );
      }
    }

    session.lastStallLogAt = 0;
    session.toolChoice = (constraints && constraints.toolChoice) || "";
    session.settleTurn = settleOnce;
    res.on("close", onClose);
    // Use writer receipt for session frame to get truthful delivery signal
    session.writePayload(formatSseData({
      type: "session",
      sessionId: session.id,
      ...(continuation ? {
        continuationReceipt: {
          contractVersion: CLIENT_TOOL_CONTRACT_VERSION,
          resultDispositions: Array.isArray(continuation.dispositions) ? continuation.dispositions : [],
          acknowledgedToolResultIds: Array.isArray(continuation.dispositions)
            ? continuation.dispositions.map((result) => result.toolCallId)
            : [],
          toolEpochState: continuation.round ? continuation.round.toolEpochState() : null,
          ...(continuation.deferredInputId ? {
            clientMessageId: continuation.deferredInputId,
            userInputStatus: String(continuation.round?.deferredInput(continuation.deferredInputId)?.state || "").toLowerCase(),
          } : {}),
          ...(continuation.deferredReceipt ? { clientMessage: continuation.deferredReceipt } : {}),
        },
      } : {}),
    }));
    session.flushPendingDeltas();

    // Keepalive through same writer (truthful signal), skip if blocked handled inside sse
    keepalive = setInterval(() => {
      try {
        session.sse({ type: "ping" });
        const now = nowMs();
        const quietMs = Math.max(0, now - session.lastSdkActivityAt);
        if (quietMs >= COMPOSER_STALL_LOG_MS
            && (session.lastStallLogAt === 0 || now - session.lastStallLogAt >= COMPOSER_STALL_LOG_MS)) {
          session.lastStallLogAt = now;
          dbg("runTurn awaiting SDK progress (observability only; no upstream timeout)",
            "session=" + session.id, "quietMs=" + quietMs, "runLive=" + (session.run !== null),
            "pending=" + session.pendingCount(), "queued=" + session.queuedToolCalls().length,
            "activeRes=" + !!session.activeRes);
        }
      } catch {}
    }, SSE_KEEPALIVE_MS);
    if (keepalive.unref) keepalive.unref();


  // driveUserSend performs a model-visible user send on the EXISTING no-timeout agent: it seeds (prepends
  // system + prior history on the FIRST send for the session), applies the enforced constraint instructions,
  // gates the advertised tools by tool_choice, attaches any images, and wires the run's completion callback
  // bound to this run's epoch (mirrors the new-user seed path). It is the single send path shared by the
  // new-user turn, the C1 mixed-turn fresh-send, and the C2/C3 re-seed — so they never drift. extraImages
  // (BR9) are merged in addition to input.images (e.g. images carried inside tool results).
  const driveUserSend = async (userText, extraImages, recoveryContext = input.recoveryContext) => {
    const deferredDelivery = deferredRound && deferredInputId
      ? deferredRound.deferredInput(deferredInputId)
      : null;
    const freshDelivery = clientMessageKind === "fresh"
      && input.freshDeliveryReceipt && typeof input.freshDeliveryReceipt === "object"
      ? input.freshDeliveryReceipt : null;
    const deliveryRecord = deferredDelivery || freshDelivery;
    const frozenDelivery = deliveryRecord
      && (freshDelivery || (deliveryRecord.deliveryAttempts || 0) > 0)
      && Object.prototype.hasOwnProperty.call(deliveryRecord, "deliveryMessage")
      && Array.isArray(deliveryRecord.deliveryAdvertise);
    const advertiseBeforeFrozenRetry = session.advertise;
    if (frozenDelivery) session.advertise = deliveryRecord.deliveryAdvertise;
    session.streamedText = "";   // reset per user turn (NOT across tool-result continuations within a run)
    session.reasonedThisRun = false; // #15: mirror streamedText — reasoning-produced tracking spans this run
    session.lastSdkActivityAt = nowMs();
    session.resetLoopBounds();   // ADD-106: a fresh send begins a new logical run -> reset the agentic-loop counters
    session.done = false;
    session.lastRunError = null;  // BR2: a fresh run starts clean; a prior run's error must not leak forward
    let agent;
    try {
      agent = await ensureAgent(session, model);
    } catch (error) {
      if (frozenDelivery) session.advertise = advertiseBeforeFrozenRetry;
      throw error;
    }
    // ensureAgent's resume/create is a network round-trip; if the client disconnected during it, onClose
    // already settled+cancelled this turn. Bail BEFORE agent.send so we don't spawn an orphan run.
    if (settled) {
      if (frozenDelivery) session.advertise = advertiseBeforeFrozenRetry;
      return;
    }
    let contextPlan = null;
    if (!frozenDelivery) {
      contextPlan = systemContextPlan(session, input);
      if (contextPlan.kind === "replace") {
        if (session.seeded && !(typeof input.history === "string" && input.history.trim())) {
          throw new ToolRoundError(
            "system_reseed_unavailable",
            "system context was replaced but bounded conversation history is unavailable for a faithful re-seed",
            410,
          );
        }
        await session.rotateForSystemReplacement(contextPlan.ids || []);
        agent = await ensureAgent(session, model);
        if (settled) return;
        contextPlan = systemContextPlan(session, input);
      }
    }
    // Build every standing/context block first. The exact current user text is
    // appended only after system/history/constraints/tool reference below, so
    // no harness-generated instruction can silently become the turn's suffix.
    let text = frozenDelivery ? (userText || "") : "";
    // H11: snapshot the seed flags so we can ROLL BACK if agent.send rejects — otherwise a failed first send
    // would leave seeded=true and the retry (reusing this in-memory session) would omit the system + history
    // prelude, answering with missing context. They are committed only after agent.send resolves.
    const seededBefore = session.seeded;
    const seededSystemBefore = session.seededSystem;
    const seededSystemBlockIdsBefore = [...session.seededSystemBlockIds];
    if (!frozenDelivery) {
      if (!session.seeded) {
        const parts = [];
        if (input.system) parts.push(input.system);
        if (input.history) parts.push("Previous conversation:\n" + input.history);
        text = parts.join("\n\n");
        session.seeded = true;
        session.seededSystem = input.system || "";
        if (contextPlan && contextPlan.ids !== null) {
          session.seededSystemBlockIds = [...contextPlan.ids];
        }
      } else if (contextPlan && contextPlan.kind === "append" && contextPlan.text) {
        // Only an exact block-ID suffix is safe to append to a warm durable
        // conversation. Replacements/reorders are handled by rotation above.
        text = "Additional system instructions:\n" + contextPlan.text;
        session.seededSystem = input.system;
        session.seededSystemBlockIds = [...contextPlan.ids];
      } else if (contextPlan && contextPlan.kind === "legacy_update" && contextPlan.text) {
        text = "Updated system instructions:\n" + contextPlan.text;
        session.seededSystem = input.system;
      } else if (contextPlan && contextPlan.ids !== null) {
        // Same-context migration from a legacy in-process aggregate: record
        // identity without emitting another model-visible instruction.
        session.seededSystem = input.system || "";
        session.seededSystemBlockIds = [...contextPlan.ids];
      }
    } else {
      // The first agent.send crossed the durable DELIVERING boundary. Reuse
      // the exact persisted SDK envelope; never rebuild it from a retry's
      // potentially changed system/history/constraints.
      session.seeded = true;
      session.seededSystem = typeof deliveryRecord.deliverySeededSystem === "string"
        ? deliveryRecord.deliverySeededSystem : session.seededSystem;
      session.seededSystemBlockIds = validSystemBlockIds(deliveryRecord.deliverySystemBlockIds)
        ? [...deliveryRecord.deliverySystemBlockIds] : session.seededSystemBlockIds;
    }
    // Enforced per-turn constraints (response_format / stop / token limit / tool_choice) as instructions.
    // H08: record the resolved tool_choice on the session so the dispatch seam can best-effort gate native
    // tools for THIS turn. H09: if a forced specific:<name> tool is not advertised, tell the model it is
    // unavailable (and effectiveAdvertise advertises nothing) instead of widening to other tools.
    const turnToolChoice = frozenDelivery
      ? (deliveryRecord.deliveryToolChoice || "")
      : (constraints && constraints.toolChoice) || "";
    session.toolChoice = turnToolChoice;
    const forcedUnavailable = forcedToolUnavailable(session.advertise, turnToolChoice);
    if (!frozenDelivery) {
      const ci = constraintInstructions({ ...constraints, forcedUnavailable });
      if (ci) text = text ? text + "\n\n" + ci : ci;
    }
    // Tool-awareness hardening (prompt mode only — the "rule" mode delivers this via requestContext.rules instead).
    // Append a manifest of EVERY offered tool (tool_choice-effective set) so composer treats the client's
    // advertised/MCP tools as first-class and CALLS them instead of self-executing. Client-agnostic.
    if (!frozenDelivery && TOOL_MANIFEST_IN_PROMPT) {
      const manifest = toolManifest(effectiveAdvertise(session.advertise, turnToolChoice));
      if (manifest) { text = text ? text + "\n\n" + manifest : manifest; dbg("toolManifest injected (prompt)", "session=" + session.id, "bytes=" + manifest.length); }
    }
    if (!frozenDelivery && typeof recoveryContext === "string" && recoveryContext.trim()) {
      const boundedRecoveryContext = recoveryContext.trim();
      text = text ? text + "\n\n" + boundedRecoveryContext : boundedRecoveryContext;
    }
    if (!frozenDelivery) {
      const currentUser = appendRulesReminder(userText);
      if (currentUser) text = text ? text + "\n\n" + currentUser : currentUser;
    }
    // Persist enough bounded, role-separated seed context at ToolRound OPEN,
    // before any client callback is handed out. A sidecar restart can then
    // rebuild a first-turn or thin-client continuation without guessing from
    // a flattened byte tail or waiting for a later deferred user message.
    const recoverySystemBlocks = normalizeSystemBlocks(input);
    session.roundRecoveryContext = {
      system: typeof input.system === "string" ? input.system : "",
      ...(recoverySystemBlocks !== null ? { systemBlocks: recoverySystemBlocks } : {}),
      history: typeof input.history === "string" ? input.history : "",
      historyFingerprint: typeof input.historyFingerprint === "string" ? input.historyFingerprint : "",
      currentUser: typeof userText === "string" ? userText : "",
      toolInventoryEpoch: typeof session.toolInventoryEpoch === "string" ? session.toolInventoryEpoch : "",
      images: Array.isArray(input.images) ? input.images : [],
    };
    // Merge images from the input with any extra images (BR9: tool-result images folded into the send).
    const allImages = frozenDelivery
      ? (deliveryRecord.deliveryHasImages ? [{}] : [])
      : [...(Array.isArray(input.images) ? input.images : []), ...(Array.isArray(extraImages) ? extraImages : [])];
    // Build the message first (toSdkImages may throw on a malformed image) BEFORE gating advertisement,
    // so a bad image never leaves session.advertise in the restricted state.
    const msg = frozenDelivery
      ? JSON.parse(canonicalJSONString(deliveryRecord.deliveryMessage))
      : (allImages.length ? { text, images: toSdkImages(allImages) } : text);
    // tool_choice gates what tools the model SEES this turn (none -> none; specific -> just that one; H09: a
    // missed forced tool -> none, never widened). Restore the full advertised set right after send: the
    // run-request advertisement is built during send, and reconcileToolName still resolves any tool the model
    // calls against the full set.
    const savedAdvertise = frozenDelivery ? advertiseBeforeFrozenRetry : session.advertise;
    const effAdv = frozenDelivery
      ? session.advertise
      : effectiveAdvertise(session.advertise, turnToolChoice);
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
    const expectedUserTextHash = sdkUserTextHash(msg);
    const sendIdempotencyKey = sdkSendIdempotencyKey(
      session,
      clientMessageId,
      clientMessageHash,
      deliveryRecord,
      clientMessageGeneration,
    );
    if (clientMessageKind === "fresh" && clientMessageId && !frozenDelivery) {
      writeFreshDeliveryReceipt(
        session.cursorKey,
        session.id,
        clientMessageId,
        clientMessageHash,
        {
          generation: clientMessageGeneration,
          agentId: session.agentId,
          idempotencyKey: sendIdempotencyKey,
          message: msg,
          advertise: effAdv,
          model,
          toolChoice: turnToolChoice,
          seededSystem: session.seededSystem || "",
          systemBlockIds: session.seededSystemBlockIds,
          hasImages: allImages.length > 0,
          identityPolicy,
        },
      );
    }
    // Send boundary: durably commit MAYBE_ACCEPTED and await fsync before any
    // agent.send. PREPARED_DURABLE recovery takes the same path. Already past
    // the boundary (MAYBE_ACCEPTED/ACCEPTED) reattaches without regressing.
    if (clientMessageKind === "fresh" && clientMessageId) {
      const current = readFreshDeliveryReceipt(
        session.cursorKey, session.id, clientMessageId, clientMessageHash,
      );
      const phase = resolveAcceptancePhase(current);
      if (phase === ACCEPTANCE_PHASE.PREPARED_DURABLE || phase === ACCEPTANCE_PHASE.NOT_SENT) {
        let maybeAccepted;
        try {
          maybeAccepted = transitionAcceptancePhase(
            session.cursorKey,
            session.id,
            clientMessageId,
            clientMessageHash,
            ACCEPTANCE_PHASE.MAYBE_ACCEPTED,
          );
        } catch (error) {
          throw new ToolRoundError(
            "acceptance_boundary_commit_failed",
            `cannot durable-commit MAYBE_ACCEPTED before agent.send: ${(error && error.message) || String(error)}`,
            503,
          );
        }
        if (!maybeAccepted
            || resolveAcceptancePhase(maybeAccepted) !== ACCEPTANCE_PHASE.MAYBE_ACCEPTED) {
          throw new ToolRoundError(
            "acceptance_boundary_commit_failed",
            "MAYBE_ACCEPTED was not fsynced; refusing to call agent.send",
            503,
          );
        }
      } else if (!sendBoundaryCrossed(phase)) {
        throw new ToolRoundError(
          "acceptance_boundary_unavailable",
          `fresh receipt phase ${phase} cannot cross the send boundary`,
          503,
        );
      }
    }
    markDeferred(DeferredInputState.DELIVERING, {
      agentId: session.agentId,
      textHash: expectedUserTextHash,
      hasImages: allImages.length > 0,
      idempotencyKey: sendIdempotencyKey,
      message: msg,
      advertise: effAdv,
      inventoryEpoch: session.toolInventoryEpoch || "",
      model,
      toolChoice: turnToolChoice,
      seededSystem: session.seededSystem || "",
      systemBlockIds: session.seededSystemBlockIds,
    });
    const observeUserMessage = (userMessage) => {
      // Ignore an unrelated or structurally drifted SDK update. agent.send's
      // successful return remains a second acceptance signal below.
      if (sdkUserTextHash(userMessage) !== expectedUserTextHash) {
        dbg("user-message-appended hash mismatch", "session=" + session.id);
        return;
      }
      markDeferred(DeferredInputState.DELIVERED, { evidence: "user_message_appended" });
      if (clientMessageKind === "fresh" && clientMessageId) {
        try {
          transitionAcceptancePhase(
            session.cursorKey, session.id, clientMessageId, clientMessageHash,
            ACCEPTANCE_PHASE.ACCEPTED, { evidence: "user_message_appended" },
          );
        } catch (error) {
          dbg("acceptance ACCEPTED (user_message_appended) unavailable; retaining prior phase",
            "session=" + session.id, (error && error.message) || String(error));
        }
      }
    };
    const sendCallbacks = {
      ...streamCallbacks(session, ep, observeUserMessage),
      ...(sendIdempotencyKey ? { idempotencyKey: sendIdempotencyKey } : {}),
    };
    try {
      await als.run({ session }, async () => {
      try {
        session.run = await agent.send(msg, sendCallbacks);
        session.sendPending = false;
        if (!session.utilityOneShot && !session.pendingRecoveryAlias
            && !sameStringArray(seededSystemBlockIdsBefore, session.seededSystemBlockIds)) {
          try {
            session.persistDurableAlias();
          } catch (aliasError) {
            // The exact frozen delivery receipt still protects an invocation
            // retry. Keep the live run available; after a process loss the
            // conservative fallback is rotate-and-reseed, never silent reuse.
            dbg("system block identity persistence unavailable; live run remains recoverable by exact receipt",
              "session=" + session.id, (aliasError && aliasError.message) || String(aliasError));
          }
        }
        try {
          session.promotePendingRecoveryAlias();
        } catch (aliasError) {
          // The ToolRound journal retains the deterministic replacement id,
          // so a restart retries this exact recovery target. Never publish a
          // premature alias or invent a second nested recovery id.
          dbg("recovery alias promotion unavailable after agent.send; retaining prior durable alias",
            "session=" + session.id, (aliasError && aliasError.message) || String(aliasError));
        }
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
            session.run = await agent.send(msg, { ...sendCallbacks, local: { force: true } });
            session.sendPending = false;
            try {
              session.promotePendingRecoveryAlias();
            } catch (aliasError) {
              dbg("recovery alias promotion unavailable after forced agent.send; retaining prior durable alias",
                "session=" + session.id, (aliasError && aliasError.message) || String(aliasError));
            }
          } catch (forceErr) {
            session.seeded = seededBefore;
            session.seededSystem = seededSystemBefore;
            session.seededSystemBlockIds = seededSystemBlockIdsBefore;
            dbg("runTurn force-retry THREW (rolled back seeded)", "session=" + session.id, (forceErr && forceErr.stack) || (forceErr && forceErr.message) || String(forceErr));
            throw forceErr;
          }
        } else {
          // H11: the send failed — roll the seed flags back to their pre-send values so a retry on this same
          // in-memory session re-prepends the system + history prelude (the first send never actually landed).
          // After MAYBE_ACCEPTED the durable phase stays MAYBE_ACCEPTED (not rejected / safely retryable).
          session.seeded = seededBefore;
          session.seededSystem = seededSystemBefore;
          session.seededSystemBlockIds = seededSystemBlockIdsBefore;
          dbg("runTurn agent.send THREW (rolled back seeded)", "session=" + session.id, "seeded->" + session.seeded, (sendErr && sendErr.stack) || (sendErr && sendErr.message) || String(sendErr));
          throw sendErr;
        }
      } finally {
        session.advertise = savedAdvertise;
      }
      if (clientMessageKind === "fresh" && clientMessageId) {
        try {
          transitionAcceptancePhase(
            session.cursorKey,
            session.id,
            clientMessageId,
            clientMessageHash,
            ACCEPTANCE_PHASE.ACCEPTED,
            { evidence: "agent_send_resolved" },
          );
        } catch (error) {
          // MAYBE_ACCEPTED is the safer fallback after the send boundary:
          // never grant a new generation or mark rejected/safely retryable.
          dbg("acceptance ACCEPTED (agent_send_resolved) unavailable; retaining MAYBE_ACCEPTED",
            "session=" + session.id, (error && error.message) || String(error));
        }
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
    } catch (error) {
      session.sendPending = false;
      // Once agent.send starts, a transport rejection cannot prove the remote
      // durable agent did not accept the message. Keep MAYBE_ACCEPTED with the
      // exact envelope/agent/key persisted above; a later continuation retries
      // only that identical logical SDK request.
      throw error;
    }
    markDeferred(DeferredInputState.DELIVERED, { evidence: "agent_send_resolved" });
  };

  // cancelStaleRun cancels a superseded run WITHOUT settling THIS turn. cancel() calls settle(), which fires
  // session.settleTurn; that handle points to this turn's settleOnce, so a naive cancel here would settle the
  // very turn we are mid-driving (driveUserSend would early-return on `settled`). Detach + restore the handle.
  // ADD-90: pass {notify:false} so cancel() does NOT release a queued new-user waiter here — that waiter must
  // not be promoted until driveUserSend has installed the replacement session.run (whose wait()->onRunComplete/
  // onRunError fires notifyLogicalDone on real completion). Otherwise the queued turn could start a SECOND
  // concurrent send on the same durable agent in the window between cancel and the replacement send. If the
  // replacement send fails, the runTurn catch path fires notifyLogicalDone as a safety net so the FIFO advances.
  const cancelStaleRun = async (terminalReason = TerminalReason.INTERRUPTED, detail = "run superseded by a new user turn") => {
    session.settleTurn = null;
    await session.cancel({ notify: false, terminalReason, detail });
    session.settleTurn = settleOnce;
    session.done = false; // cancel() set done=true; clear it so a subsequent driveUserSend wires completion
  };

    dbg("runTurn START", "session=" + session.id, "inputType=" + input.type, "turnToken=" + session.turnToken);
    // A genuine new user turn starts a new detection scope. Tool-result
    // continuations and restart recovery keep the current scope so a late
    // internal Write/Read cannot evade the guard by crossing an HTTP turn.
    if (input.type === "user" && !session.recoverySourceRound) {
      session.resetSyntheticArtifactGuard(input.text || "");
    } else if (input.type === "user" && session.recoverySourceRound && Array.isArray(input.results)) {
      for (const result of input.results) {
        const call = session.recoverySourceRound.calls.get(result && result.toolCallId);
        session.rememberLargeMcpResult(result && result.content, call && call.name);
      }
    }
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
      // Rotation cancellation belongs to the old durable agent, not to this
      // newly-opened HTTP turn. Detach the current settle callback while the
      // old handle closes; otherwise cancel() settles this response before the
      // fallback send starts and driveUserSend correctly refuses to create an
      // orphan, yielding a session frame plus empty [DONE].
      session.settleTurn = null;
      try { await session.rotateForModelChange(model); }
      finally { session.settleTurn = settleOnce; }
      if (clientMessageId) {
        session.activeClientMessageId = clientMessageId;
        session.activeClientMessageHash = clientMessageHash;
        session.activeClientMessageGeneration = clientMessageGeneration;
        session.activeClientMessageKind = clientMessageKind;
        session.activeIdentityPolicy = identityPolicy;
        session.sendPending = true;
      }
      if (deferredInputId) session.activeDeferredInputId = deferredInputId;
      session.beginReplaySegment(clientMessageId);
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
        if (session.seeded && !(typeof input.history === "string" && input.history.trim())) {
          throw new ToolRoundError(
            "history_reseed_unavailable",
            "conversation history changed but bounded replacement history is unavailable for a faithful re-seed",
            410,
          );
        }
        session.settleTurn = null;
        try { await session.rotateForHistoryReplacement(); }
        finally { session.settleTurn = settleOnce; }
        if (clientMessageId) {
          session.activeClientMessageId = clientMessageId;
          session.activeClientMessageHash = clientMessageHash;
          session.activeClientMessageGeneration = clientMessageGeneration;
          session.activeClientMessageKind = clientMessageKind;
          session.activeIdentityPolicy = identityPolicy;
          session.sendPending = true;
        }
        if (deferredInputId) session.activeDeferredInputId = deferredInputId;
        session.beginReplaySegment(clientMessageId);
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
    } else if (input.type === "tool_results" && continuation) {
      const round = continuation.round;
      if (!round || round.sessionId !== session.id) throw new ToolRoundError("round_lost", "continuation round is not owned by this Session", 410);
      const committedIds = continuation.committedIds || [];
      const recoveryPlan = toolResultRecoveryPlan(round, input, session);

      if (recoveryPlan.requiresFreshRecovery && recoveryPlan.remainingUnreceipted > 0) {
        // Do not resolve only part of a parallel callback wave and do not
        // cancel its unanswered siblings. Receipts are already durable; a
        // later incremental continuation will recover from the complete set.
        // A REGISTERED sibling is part of the same callback wave. Hand it on
        // this continuation response before the tool_use terminal instead of
        // cancelling/rotating the live run with an invisible obligation.
        session.flushJournaledCalls();
        const receipt = session.sseReceipt({
          type: "turn_end",
          stop_reason: "tool_use",
          receipt: "partial_results_deferred_for_fidelity",
          outstandingToolCalls: recoveryPlan.remainingUnreceipted,
        });
        if (receipt.ok) receipt.handedToNode.then(() => session.settle()).catch(() => {});
      } else if (recoveryPlan.requiresFreshRecovery) {
        const detail = "tool continuation redirected to a faithful fresh send because the paused run could not accept all new payload";
        if (round.state !== RoundState.TERMINAL) round.terminalize(TerminalReason.INTERRUPTED, detail);
        await cancelStaleRun(TerminalReason.INTERRUPTED, detail);
        const redirectedAgentId = `${session.id}_redirect_${round.route.slice(0, 10)}`;
        writeDurableAgentAlias(session.cursorKey, session.id, redirectedAgentId, session);
        session.agentId = redirectedAgentId;
        session.resetSeedState();
        const redirected = buildResultRecoveryInput(round, input);
        // driveUserSend already merges input.images (the trailing user
        // payload); pass only images recovered from durable tool receipts so
        // the user images are not duplicated.
        await driveUserSend(redirected.text, recoveryPlan.resultImages, redirected.recoveryContext);
      } else {
        const applied = round.applyCommittedResults(committedIds);
        if (applied.length > 0) session.lastSdkActivityAt = nowMs();
        const remaining = round.pendingCount;
        dbg("runTurn durable tool_results", "session=" + session.id, "committed=" + committedIds.length,
          "applied=" + applied.length, "remaining=" + remaining, "runLive=" + (session.run !== null));
        if (session.flushJournaledCalls()) {
          // A newly emitted REGISTERED call now owns the response and will pause it through receipt ordering.
        } else if (remaining > 0) {
          const receipt = session.sseReceipt({ type: "turn_end", stop_reason: "tool_use", receipt: "partial_results_applied" });
          if (receipt.ok) receipt.handedToNode.then(() => session.settle()).catch(() => {});
        } else if (applied.length === 0 && session.run === null) {
          const receipt = session.sseReceipt({ type: "turn_end", stop_reason: "end_turn", receipt: "already_applied" });
          if (receipt.ok) receipt.handedToNode.then(() => session.settle()).catch(() => {});
        }
        // When the last callback was applied to a live run, resolving it resumes the SDK. Its next delta, tool
        // wave, or run terminal settles this response; no synthetic terminal is emitted here.
      }
    } else if (session.run) {
      // Re-entrancy guard: a new user turn while a run is still in flight (paused awaiting tools)
      // would spawn a second concurrent SDK run and orphan the first. CLIProxy should serialize
      // turns per sessionId; reject here as a backstop.
      dbg("runTurn RE-ENTRANT new user turn while run in flight -> reject", "session=" + session.id);
      session.sse({
        type: "turn_end",
        stop_reason: "error",
        error: "a turn is already in progress for this session",
        _clientLeaseTerminal: false,
      });
      settleOnce();
    } else {
      await driveUserSend(input.text || "", null);
    }
    // C2/BR-C2: record the fingerprint of the history we just seeded/continued, so a LATER changed
    // fingerprint (a future /compact) is detected. Always update on a successful seed/continue. H12: ALSO
    // persist it durably keyed by agentId so a COLD restart can detect a compact that happened while the
    // bridge was down (the in-memory fp is lost on restart; the durable one survives).
    if (inboundFp && !session.utilityOneShot) {
      session.historyFingerprint = inboundFp;
      writeDurableFingerprint(session.cursorKey, session.agentId || session.id, inboundFp);
    }
    await turnSettled;
  } catch (e) {
    dbg("runTurn CATCH exception", "session=" + session.id, (e && e.stack) || (e && e.message) || String(e));
    const turnFailure = (e && e.message) || String(e);
    releasePreReservationWithoutReceipt(input);
    session.rejectAllPending(`turn failed: ${turnFailure}`, TerminalReason.RUN_ERROR);
    if (!settled) session.sse({
      type: "turn_end",
      stop_reason: "error",
      error: (e && e.message) || String(e),
      ...(e && e.code === "local_replay_capacity" ? {
        errorCode: "local_replay_capacity",
        retryable: true,
      } : {}),
    });
    // ADD-101: ALWAYS settle the turn on an error so the per-turn latch (turnSettled) resolves — otherwise a
    // throw before any settle() call leaves the latch unresolved (queue/lifecycle stalls). settle() is idempotent.
    session.settle();
    // ADD-90 safety net (guarded): if this throw aborted a C1/reseed replacement send AFTER cancelStaleRun
    // (which used notify:false) and NO run was installed, nothing will fire onRunComplete to release a queued
    // waiter -> release it here so the FIFO does not hang. But ONLY when no run is live: if a run WAS installed,
    // its wait()->onRunComplete/onRunError will notify on REAL completion, and releasing a waiter early here
    // would re-introduce the very ADD-90 race (the queued turn starting concurrently with the live run).
    if (session.run !== null) await session.cancel({ terminalReason: TerminalReason.RUN_ERROR, detail: turnFailure });
    else session.notifyLogicalDone();
    // ADD-61: an EMPTY newly-created session whose first turn failed before any AGENT was even established
    // (getPlatform/ensureAgent threw — a transient platform-init failure, or the ADD-53 key-collision reject)
    // must NOT linger in the sessions map. While present it pins its platform (platformHasSession) and blocks
    // idle eviction. Delete it ONLY when it is truly empty AND no agent was created: never seeded, no agent,
    // no live/paused run, no pending tools, no queued waiters. The `session.agent === null` guard is the line
    // between this case and H11 (agent.send failed AFTER ensureAgent succeeded -> session.agent IS set): H11
    // deliberately KEEPS the session so the retry reuses the cached agent and re-prepends system + history.
    if (!session.seeded && session.agent === null && session.run === null && session.pendingCount() === 0 && !session.hasQueuedWaiters() && sessions.get(session.id) === session) {
      dbg("runTurn drop empty failed-first-turn session (ADD-61)", "session=" + session.id);
      sessions.delete(session.id);
      cancelSessionDetached(session, { terminalReason: TerminalReason.RUN_ERROR, detail: "failed first turn" });
    }
  } finally {
    session.sendPending = false;
    if (session.run === null && session.activeDeferredInputId === deferredInputId) {
      session.activeDeferredInputId = "";
    }
    if (session.run === null && session.activeClientMessageId === clientMessageId) {
      session.activeClientMessageId = "";
      session.activeClientMessageHash = "";
      session.activeClientMessageGeneration = 1;
      session.activeClientMessageKind = "";
      session.activeIdentityPolicy = TURN_IDENTITY_POLICY.NONE;
    }
    clearInterval(keepalive);
    res.off("close", onClose);
    if (session.activeRes === res) {
      try { await session.finishResponse(); }
      catch (error) { dbg("finishResponse failed", "session=" + session.id, (error && error.message) || String(error)); }
      finally { session.activeRes = null; session.responseWriter = null; }
    }
    if (session.settleTurn === settleOnce) session.settleTurn = null;
  }
}

async function handleHttpRequest(req, res) {
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
  if (req.method === "GET" && req.url === "/ready") {
    res.writeHead(bridgeReady ? 200 : 503, { "Content-Type": "application/json", "Cache-Control": "no-store" });
    res.end(JSON.stringify(readinessBody()));
    return;
  }
  // The SDK-only MCP shim is never an external API. The socket peer must be
  // loopback and forwarded requests are refused even when they know a session id.
  const mcpRoute = classifyMcpRoute(req);
  if (mcpRoute) {
    if (mcpRoute.error) {
      res.writeHead(mcpRoute.status, { "Content-Type": "application/json" });
      res.end(JSON.stringify({ error: mcpRoute.error }));
      return;
    }
    await handleMcp(req, res, mcpRoute.sessionId, mcpRoute.serverKey); return;
  }
  if (req.method === "POST" && (req.url === "/agent/turn" || req.url === "/agent/continue")) {
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
    if (req.url === "/agent/continue") await handleContinue(req, res, body, cursorKey);
    else await handleTurn(req, res, body, cursorKey);
    return;
  }
  res.writeHead(404); res.end(JSON.stringify({ error: "not found" }));
}

async function handleHttpRequestSafely(req, res, dispatch = handleHttpRequest) {
  // Node does not observe a rejected Promise returned by an async request
  // listener. Contain every request-local transport/backpressure failure here
  // so a client closing a replay socket cannot reach the process-wide fatal
  // unhandledRejection handler and restart the bridge for every other client.
  try {
    await dispatch(req, res);
  } catch (error) {
    dbg("request-local bridge failure contained", req.method, req.url,
      (error && error.stack) || (error && error.message) || String(error));
    if (!res.headersSent) {
      try {
        res.writeHead(500, { "Content-Type": "application/json" });
        res.end(JSON.stringify({ error: "request failed" }));
      } catch {}
      return false;
    }
    if (await finishStartedSseWithError(res, {
      receipt: "request_failure_after_headers",
      retryable: false,
      error: "the bridge request failed after its response stream started",
    })) return false;
    try { if (!res.writableEnded) res.destroy(); } catch {}
    return false;
  }
  return true;
}

const server = createServer((req, res) => {
  void handleHttpRequestSafely(req, res);
});

// ---- idle session eviction (bounded sessions Map; no leaked agents) ----
const evictTimer = setInterval(() => {
  const cut = nowMs() - SESSION_TTL_MS;
  for (const [id, s] of sessions) {
    if (!s.activeRes && !s.run && !s.hasQueuedWaiters() && s.lastActivity < cut) {
      sessions.delete(id);
      cancelSessionDetached(s, { terminalReason: TerminalReason.SESSION_EVICTED, detail: "idle session eviction" });
    }
  }
  // Multi-tenant only: dispose idle per-user platforms. Single-tenant keeps its single platform resident
  // (it is the common, hot path) — it is never evicted, matching the pre-pool behavior exactly.
  if (MULTI_TENANT) {
    const pcut = nowMs() - PLATFORM_TTL_MS;
    for (const [h, entry] of platforms) {
      if (entry.lastUsed < pcut && !platformHasSession(h)) {
        platforms.delete(h);
        upstreamBreaker.delete(h);
        void disposePlatform(entry);
      }
    }
  }
  const now = nowMs();
  for (const [h, breaker] of upstreamBreaker) {
    if (now >= breaker.openUntil && !platforms.has(h) && !platformHasSession(h)) {
      upstreamBreaker.delete(h);
    }
  }
}, 60000);
if (evictTimer.unref) evictTimer.unref();

// ---- graceful shutdown: stop accepting, settle/cancel sessions, close stores ----
let shuttingDown = false;
// CURSOR_AGENT_DRAIN_MS: on SIGTERM/SIGINT (a redeploy), stop accepting NEW turns (server.close) and let any
// IN-FLIGHT runs finish for up to this BOUND before force-cancelling — so a restart does not kill a turn that was
// about to finish (the orphaned-tool_call_id / lost-continuation class). BOUNDED so a redeploy waits AT MOST this
// long, never indefinitely (set 0 to restore the old immediate-cancel behavior). Kept under a typical container
// SIGTERM grace window so the platform never SIGKILLs us first.
const DRAIN_MS = envInt("CURSOR_AGENT_DRAIN_MS", 20000, { min: 0 });
const PLATFORM_DRAIN_MAX_MS = envInt("CURSOR_AGENT_PLATFORM_DRAIN_MAX_MS", 30000, { min: 2000, max: 600000 });
const REQUESTED_SHUTDOWN_MAX_MS = envInt("CURSOR_AGENT_SHUTDOWN_MAX_MS", 28000, { min: 1000, max: 600000 });
const SHUTDOWN_MAX_MS = Math.min(REQUESTED_SHUTDOWN_MAX_MS, PLATFORM_DRAIN_MAX_MS - 1000);
const SHUTDOWN_CANCEL_CONCURRENCY = envInt("CURSOR_AGENT_SHUTDOWN_CANCEL_CONCURRENCY", 16, { min: 1, max: 128 });
async function runBoundedShutdownTasks(tasks, deadline, concurrency = SHUTDOWN_CANCEL_CONCURRENCY) {
  let next = 0;
  const workers = Array.from({ length: Math.min(concurrency, tasks.length) }, async () => {
    while (next < tasks.length && nowMs() < deadline) {
      const index = next++;
      try { await tasks[index](); } catch {}
    }
  });
  if (workers.length === 0) return;
  const remaining = Math.max(0, deadline - nowMs());
  await Promise.race([
    Promise.allSettled(workers),
    new Promise((resolve) => setTimeout(resolve, remaining)),
  ]);
}
async function flushProcessDiagnostics() {
  const flush = (stream) => new Promise((resolve) => {
    try { stream.write("", resolve); } catch { resolve(); }
  });
  await Promise.all([flush(process.stdout), flush(process.stderr)]);
}
async function shutdown(exitCode = 0, drain = true) {
  if (shuttingDown) return; shuttingDown = true;
  const startedAt = nowMs();
  const shutdownDeadline = startedAt + SHUTDOWN_MAX_MS;
  // Planned and fatal cleanup share one global deadline below Railway's drain
  // window. A wedged cancel/store dispose can never invite platform SIGKILL
  // before terminal journals and other sessions get their cleanup opportunity.
  const forcedExit = setTimeout(() => process.exit(exitCode), SHUTDOWN_MAX_MS);
  bridgeReady = false;
  try { server.close(); } catch {} // refuse NEW connections; in-flight ones keep streaming
  if (drain && DRAIN_MS > 0) {
    const cleanupReserve = Math.min(7000, Math.floor(SHUTDOWN_MAX_MS / 2));
    const deadline = Math.min(startedAt + DRAIN_MS, shutdownDeadline - cleanupReserve);
    const anyLive = () => { for (const [, s] of sessions) { if (s && s.run && !s.done) return true; } return false; };
    try { while (anyLive() && nowMs() < deadline) { await new Promise((r) => setTimeout(r, 250)); } } catch {}
  }
  // Persist every open round's terminal state before awaiting any SDK cleanup;
  // one stuck close must not prevent later sessions from being journaled.
  const terminalDrains = [];
  for (const [, s] of sessions) {
    try { s.rejectAllPending("bridge shutdown", TerminalReason.SHUTDOWN); } catch {}
    try {
      const receipt = s.emitCancellationTerminalReceipt(TerminalReason.SHUTDOWN, "bridge shutdown");
      if (receipt?.ok) terminalDrains.push(Promise.resolve(s.finishResponse()).catch(() => {}));
    } catch {}
  }
  // Start every writer drain before waiting. A bounded worker queue could let
  // one group of wedged sockets prevent tail sessions from ever receiving a
  // terminal. SDK cancellation happens only after this fast all-session pass.
  if (terminalDrains.length > 0) {
    const terminalDeadline = Math.max(nowMs(), shutdownDeadline - 5000);
    await Promise.race([
      Promise.allSettled(terminalDrains),
      new Promise((resolve) => setTimeout(resolve, Math.max(0, terminalDeadline - nowMs()))),
    ]);
  }
  const cancelTasks = [];
  for (const [, s] of sessions) {
    // A cancellation-abandonment guard may have initiated this shutdown from
    // inside the same cancelPromise. Never self-await that wedged promise; its
    // round was terminal-journaled above and the process is exiting.
    if (s.cancelPromise) continue;
    cancelTasks.push(() => s.cancel({ terminalReason: TerminalReason.SHUTDOWN, detail: "bridge shutdown" }));
  }
  await runBoundedShutdownTasks(cancelTasks, shutdownDeadline - 1000);
  sessions.clear();
  const disposeTasks = [...platforms.values()].map((entry) => () => disposePlatform(entry));
  await runBoundedShutdownTasks(disposeTasks, shutdownDeadline - 250, SHUTDOWN_CANCEL_CONCURRENCY);
  platforms.clear();
  clearTimeout(forcedExit);
  await flushProcessDiagnostics();
  process.exit(exitCode);
}
process.on("SIGTERM", () => { void shutdown(0, true); });
process.on("SIGINT", () => { void shutdown(0, true); });

// CRASH BACKSTOP: an unclassified exception means process invariants are no
// longer trustworthy. Mark readiness false, terminal-journal every open round,
// and exit so the parent supervisor restarts this sidecar in isolation.
process.on("uncaughtException", (err, origin) => {
  try { console.error("[cursor-agent-bridge] FATAL uncaughtException; terminalizing rounds and exiting origin=" + origin + ":", (err && err.stack) ? err.stack : err); } catch { /* a logger throw must never re-crash the handler */ }
  void shutdown(1, false);
});
// These exact errors are leaked by @cursor/sdk's internal cancellation
// iterators. None carries a session id or cancel generation, so they must never
// be attributed by looking at whichever session happens to be live later.
function isClosedInputStreamError(reason) {
  return !!reason && (reason.name === "WriteIterableClosedError"
    || /WritableIterable is closed/i.test(String(reason.message || "")));
}
function isSdkIteratorClosedError(reason) {
  return !!reason && /^(?:Iterator was closed|AsyncIterator was closed)$/i.test(String(reason.message || "").trim());
}
// @cursor/sdk surfaces an expected run.cancel() as an unhandled DOM
// AbortError from its internal abort listener rather than as the promise that
// cancel() returns. One cancellation can leak several rejection shapes, so a
// ticket permits a small bounded sequence rather than disappearing after the
// first error. Tickets are short-lived, pruned on every insertion/use, and
// hard-capped so successful cancellations that leak nothing cannot grow this
// process-local list forever.
const expectedSdkAbortTickets = [];
const EXPECTED_SDK_TICKET_TTL_MS = 10_000;
const EXPECTED_SDK_TICKET_MAX_MATCHES = 8;
const EXPECTED_SDK_TICKET_MAX_ENTRIES = 1024;
function pruneExpectedSdkAbortTickets(now = nowMs()) {
  for (let i = expectedSdkAbortTickets.length - 1; i >= 0; i--) {
    const ticket = expectedSdkAbortTickets[i];
    if (ticket.expiresAt <= now || ticket.matchesRemaining <= 0) expectedSdkAbortTickets.splice(i, 1);
  }
  if (expectedSdkAbortTickets.length > EXPECTED_SDK_TICKET_MAX_ENTRIES) {
    expectedSdkAbortTickets.splice(0, expectedSdkAbortTickets.length - EXPECTED_SDK_TICKET_MAX_ENTRIES);
  }
}
function noteExpectedSdkAbort(sessionId = "", runEpoch = null) {
  const now = nowMs();
  pruneExpectedSdkAbortTickets(now);
  expectedSdkAbortTickets.push({
    sessionId: String(sessionId || ""),
    runEpoch: Number.isSafeInteger(runEpoch) ? runEpoch : null,
    expiresAt: now + EXPECTED_SDK_TICKET_TTL_MS,
    matchesRemaining: EXPECTED_SDK_TICKET_MAX_MATCHES,
  });
  pruneExpectedSdkAbortTickets(now);
}
function consumeExpectedSdkAbort(sessionsMap = sessions) {
  const now = nowMs();
  pruneExpectedSdkAbortTickets(now);
  const index = expectedSdkAbortTickets.findIndex((ticket) => {
    const session = sessionsMap && typeof sessionsMap.get === "function"
      ? sessionsMap.get(ticket.sessionId) : null;
    // Session.cancel marks done before calling run.cancel. If the session has
    // already been removed, that cancellation is also unambiguously complete.
    return !session
      || session.done
      || session.run === null
      || (ticket.runEpoch !== null
        && Number.isSafeInteger(session.runEpoch)
        && session.runEpoch > ticket.runEpoch);
  });
  if (index < 0) return false;
  expectedSdkAbortTickets[index].matchesRemaining--;
  pruneExpectedSdkAbortTickets(now);
  return true;
}
// Cursor's SDK can leak WriteIterableClosedError / "Iterator was closed" from
// internal async iterators after run.cancel()/agent.close(), outside the
// promise being awaited. The rejection carries no session id. A recent
// explicit-cancel ticket is weak temporal evidence for this SDK lifecycle
// family. The unhandled-rejection policy may use it only when no live SDK work
// exists; otherwise it restarts rather than letting a global ticket mask an
// unrelated failure.
function consumeExpectedSdkLifecycleClosure(sessionsMap = sessions) {
  return consumeExpectedSdkAbort(sessionsMap);
}
function isSdkAbortError(reason) {
  return !!reason && (reason.name === "AbortError" || /operation was aborted/i.test(String(reason.message || "")));
}
function currentSdkCancellationContext() {
  const store = als.getStore();
  const context = store && store.sdkCancellation;
  if (!context || typeof context.sessionId !== "string" || !Number.isSafeInteger(context.runEpoch)) return null;
  return context;
}
function hasLiveSdkWork(sessionsMap = sessions) {
  if (!sessionsMap || typeof sessionsMap.values !== "function") return false;
  for (const session of sessionsMap.values()) {
    if (session && !session.done && (session.run || session.sendPending)) return true;
  }
  return false;
}

let fatalRejectionShutdownStarted = false;
process.on("unhandledRejection", (reason) => {
  try {
    const sdkLifecycleKind = isSdkAbortError(reason) ? "AbortError"
      : isClosedInputStreamError(reason) ? "input-stream closure"
        : isSdkIteratorClosedError(reason) ? "iterator closure"
          : null;
    if (sdkLifecycleKind) {
      const exactCancellation = currentSdkCancellationContext();
      if (exactCancellation) {
        console.error("[cursor-agent-bridge] expected SDK cancellation lifecycle rejection ignored session="
          + exactCancellation.sessionId + " runEpoch=" + exactCancellation.runEpoch + " kind=" + sdkLifecycleKind + ":",
        (reason && reason.message) || reason);
        return;
      }
      const temporalTicket = consumeExpectedSdkLifecycleClosure(sessions);
      if (temporalTicket && !hasLiveSdkWork(sessions)) {
        // Some SDK builds lose async context when forwarding cleanup through a
        // native/event-emitter boundary. It is safe to use the bounded temporal
        // fallback only when no run can be stranded or mistaken for the source.
        console.error("[cursor-agent-bridge] expected SDK cancellation lifecycle rejection ignored with no live SDK work kind="
          + sdkLifecycleKind + ":", (reason && reason.message) || reason);
        return;
      }
      // With live work and no exact async context, neither the global ticket nor
      // a "sole live session" heuristic can identify the source. Restart the
      // isolated sidecar: this avoids both killing an arbitrary session and
      // swallowing a genuine failure that would strand a run forever.
      console.error("[cursor-agent-bridge] " + (temporalTicket ? "ambiguous" : "unattributed")
        + " SDK " + sdkLifecycleKind + "; restarting isolated sidecar:", (reason && reason.message) || reason);
      void shutdown(1, false);
      return;
    }
    // Both a rate-limit flood (NGHTTP2_ENHANCE_YOUR_CALM) and an auth rejection ([unauthenticated], the session
    // token went stale/invalid mid-connection) POISON the ONE cached HTTP/2 connection: every reused stream on it
    // then keeps failing the same way until the platform is recycled, and getPlatform only evicts a REJECTED
    // create — never a live-but-poisoned one. Same recovery for both: drop + dispose the poisoned platform so the
    // NEXT turn dials a FRESH connection (which also re-mints auth), and OPEN the per-key breaker so client
    // retries back off (exponential) instead of immediately re-poisoning it. A successful run closes the breaker.
    const poison = isUpstreamRateLimit(reason) ? "rate-limit (ENHANCE_YOUR_CALM)"
      : isUpstreamUnauthenticated(reason) ? "auth rejected (unauthenticated)"
        : null;
    if (poison) {
      const kh = rateLimitedKeyToRecycle(sessions, platforms);
      if (kh) {
        const affected = terminalizePoisonedPlatformSessions(kh, reason);
        recyclePlatform(kh);
        const e = tripBreaker(kh);
        console.error("[cursor-agent-bridge] upstream " + poison
          + " -> terminalized affected sessions=" + affected
          + " + recycled connection (force re-auth/re-dial) + breaker OPEN key=" + kh
          + " fails=" + e.fails + " ~" + Math.ceil(breakerRetryAfterMs(kh) / 1000) + "s:",
        (reason && reason.message) || reason);
      } else {
        // Multiple live tenants and no async attribution means continuing could
        // strand an arbitrary run on a poisoned shared SDK invariant. Restart
        // only this sidecar; the parent supervisor keeps the API available.
        console.error("[cursor-agent-bridge] upstream " + poison
          + " could not be attributed safely in multi-tenant mode -> restarting isolated sidecar:",
        (reason && reason.message) || reason);
        void shutdown(1, false);
      }
      return;
    }
  } catch { /* never throw from the handler */ }
  if (fatalRejectionShutdownStarted) {
    try { console.error("[cursor-agent-bridge] additional unhandledRejection during fatal shutdown ignored:", (reason && reason.stack) ? reason.stack : reason); } catch {}
    return;
  }
  fatalRejectionShutdownStarted = true;
  try { console.error("[cursor-agent-bridge] FATAL unhandledRejection; terminalizing rounds and exiting:", (reason && reason.stack) ? reason.stack : reason); } catch { /* never throw from the handler */ }
  void shutdown(1, false);
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
  const st = { path: "/x", command: "ls", workingDirectory: "/sdk-selftest", fileText: "alpha\nbeta", offset: 0, limit: 20, returnFileContentAfterWrite: true };
  const ctx = { cwd: "/sdk-selftest" };
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
  for (const cas of ["readArgs", "redactedReadArgs", "writeArgs", "deleteArgs", "shellArgs"]) add(resultCaseFor(cas), "blocked:" + cas, blockedNativeResult(cas, st, "/sdk-selftest").__ccJson);
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
  // Validate the opt-in McpImageContent variant ({image:{data:<base64>,mimeType}}) through the REAL proto.
  // Serialization compatibility is necessary but not sufficient for a model route to consume it; the default
  // image path therefore remains durable fresh-send recovery.
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
  if (RAILWAY_RUNTIME && !STATE_ROOT_ON_RAILWAY_VOLUME) {
    console.error(`[bridge] Railway runtime requires STATE_ROOT beneath RAILWAY_VOLUME_MOUNT_PATH (stateRoot=${STATE_ROOT_RESOLVED}, volume=${RAILWAY_VOLUME_ROOT || "<missing>"}); refusing ephemeral round state`);
    process.exit(1);
  }
  // ADD-105: validate the bind host BEFORE doing any work. A non-loopback bind without multi-tenant auth is
  // refused (fail-closed) unless explicitly overridden; a non-loopback bind always warns about plaintext exposure.
  const bindCheck = validateBindHost(BRIDGE_HOST, MULTI_TENANT, ALLOW_INSECURE_BIND);
  if (!bindCheck.ok) { console.error("[bridge]", bindCheck.error); process.exit(1); }
  if (bindCheck.warn) console.warn("[bridge]", bindCheck.warn);
  // Refuse readiness unless STATE_ROOT supports the exact journal durability primitives. The empty cwd is
  // only SDK initialization context; it is never advertised as the client's workspace or used for tools.
  try {
    mkdirSync(EMPTY_CWD, { recursive: true });
    accessSync(STATE_ROOT, constants.W_OK);
    probeStateRoot(STATE_ROOT);
    const { journal } = getRoundInfrastructure();
    // Nonterminal rounds may still be owned by an overlapping replica or be
    // awaiting an exact client continuation after restart. Keep them durable;
    // signed continuation recovery, not process startup, decides their fate.
    journal.cas.sweepDirectory(journal.dir, { orphanAgeMs: UNRESOLVED_RESERVATION_ORPHAN_MS });
    receiptCAS.sweepDirectory(COMPLETED_TURN_DIR, { orphanAgeMs: UNRESOLVED_RESERVATION_ORPHAN_MS });
    journal.cleanupTerminal({ ttlMs: TERMINAL_ROUND_TTL_MS, maxTerminal: TERMINAL_ROUND_MAX });
    cleanupCompletedTurnReceipts({ ttlMs: TERMINAL_ROUND_TTL_MS, maxTerminal: TERMINAL_ROUND_MAX });
    reconcileUnresolvedReservations();
  }
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
    .then(() => {
      bridgeReady = true;
      const maintenance = setInterval(() => {
        try { runDurableMaintenance(); }
        catch (error) {
          console.error("[cursor-agent-bridge] durable maintenance failed; preserving state:",
            (error && error.message) || String(error));
        }
      }, DURABLE_MAINTENANCE_MS);
      maintenance.unref();
      server.listen(PORT, BRIDGE_HOST, () => console.log(`[cursor-agent-bridge] listening on http://${BRIDGE_HOST}:${PORT} (ready, patched CJS, native-unreachable + bundle-seam + result-serialization self-tests passed, durable stateRoot=${STATE_ROOT})`));
    })
    .catch((e) => { console.error("[bridge]", (e && e.message) || e); process.exit(1); });
}

export { runDurableMaintenance, runSdkAgentMaintenance, sdkAgentGCRoots };
export { CC_CASES, composerModelSelection, headlessRequestContext, headlessMcpState, Session, AdvertisedToolRegistry, reconcileExport, toSdkImages, constraintInstructions, effectiveAdvertise, forcedToolUnavailable, nativeToolBlockedByChoice, toolManifest, toolManifestRule, blockedNativeResult, blockedSyntheticNativeExecIfNeeded, typedUnavailableResult, mcpDispatchResult, TYPED_UNAVAILABLE_U, parseShellContent, streamCallbacks, ccToolId, authorizeRequest, authorizeRequestWith, platformHasSession, keyHash, loadSdk, selfTestNativeUnreachable, selfTestBundleSeam, selfTestResultSerialization, handleTurn, handleContinue, handleHttpRequestSafely, buildRestartRecoveryInput, continuationTenantMismatch, completedTurnRequestHash, validCompletedTurnReceipt, sdkSendIdempotencyKey, turnInvocationIdentity, cleanupCompletedTurnReceipts, readFreshDeliveryReceipt, writeFreshDeliveryReceipt, transitionFreshAttemptState, writeCompletedTurnReceipt, stateRootDiskStatus, sessions, liveToolRounds, completedTurnReceipts, isClosedInputStreamError, isSdkIteratorClosedError, isUpstreamRateLimit, isUpstreamUnauthenticated, recyclePlatform, terminalizePoisonedPlatformSessions, tripBreaker, breakerOpen, breakerRetryAfterMs, closeBreaker, breakerBackoffMs, soleStreamingSession, rateLimitedKeyToRecycle, upstreamBreaker, platforms, collectToolResultImages, isConversationTooLong, ensureAgent, buildMcpServers, mcpServerKeyForTool, mcpToolsForServer, mcpDescriptorsForServer, mcpDispatch, dispatchMcpBatch, handleMcp, MCP_GROUPING, MCP_SHIM_ENABLED, CLIENT_TOOL_PROVIDER_ID, DEFAULT_MCP_SERVER_KEY, readBodyBounded, PayloadTooLargeError, MAX_AGENT_TURN_BYTES, MAX_SSE_FRAME_BYTES, sseFrameSizeError, envInt, composerWorkspaceCwd, buildReadSuccess, buildWriteSuccess, healthBody, readinessBody, isLoopbackRemote, classifyMcpRoute, getPlatform, keyFingerprint, PlatformKeyCollisionError, MAX_SESSIONS, MAX_PLATFORMS, wrapToolInput, truncateLiveToolResult, validateBindHost, resolveBridgeHost, bindHostIsLoopback, syntheticAgentArtifactRequest, syntheticAgentArtifactFailure, COMPOSER_LIVE_TOOL_RESULT_MAX_BYTES, COMPOSER_SCHEMA_INLINE_MAX_BYTES, COMPOSER_OUT_QUEUE_MAX_BYTES, COMPOSER_MAX_TOOL_ROUNDS, COMPOSER_MAX_REPEAT_TOOL, COMPOSER_MAX_INVALID_TOOL_CALLS, COMPOSER_MAX_IDENTICAL_INVALID_TOOL_CALLS, augmentUnderspecifiedToolSchema, normalizeToolArgsToSchema, extractScalarFromWrapper, argContractFor, augmentToolDescription, augmentWorkflowResultOnFailure, augmentBackgroundLaunchResult, snapWorkflowAgentTypes, appendRulesReminder, prepareAdvertisedToolRegistry, refreshSessionFromBody, sdkAdvertisedTools, mcpImageResultsEnabled, normalizeSystemBlocks, systemContextPlan, toolResultRecoveryPlan, runBoundedShutdownTasks, isSdkAbortError, noteExpectedSdkAbort, consumeExpectedSdkAbort, consumeExpectedSdkLifecycleClosure, replayMemoryBudget, mutateTurnReceipt };
export {
  TURN_RECEIPT_VERSION,
  ACCEPTANCE_PHASE,
  resolveAcceptancePhase,
  migrateLegacyAcceptancePhase,
  transitionAcceptancePhase,
  IllegalAcceptanceTransition,
  EnvelopeMutationError,
  isLegalAcceptanceTransition,
  assertLegalAcceptanceTransition,
  recoveryActionForPhase,
};
function reconcileExport(advertise, want) { const s = new Session("x"); s.advertise = advertise; return s.reconcileToolName(want); }
