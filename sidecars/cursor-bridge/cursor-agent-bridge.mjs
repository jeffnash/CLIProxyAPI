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
//      CURSOR_AGENT_SESSION_TTL_MS (default 1800000 idle session eviction).

import { createServer } from "node:http";
import { randomUUID, timingSafeEqual, createHash } from "node:crypto";
import { fileURLToPath } from "node:url";
import { createRequire } from "node:module";
import { AsyncLocalStorage } from "node:async_hooks";
import { readFileSync, mkdirSync, accessSync, constants, writeSync } from "node:fs";
import path from "node:path";

const PORT = parseInt(process.env.CURSOR_AGENT_BRIDGE_PORT || "9798", 10);
const API_KEY = process.env.CURSOR_API_KEY || "";
const STATE_ROOT = process.env.CURSOR_AGENT_STATE_ROOT || path.join(process.cwd(), ".cursor-agent-store");
const EMPTY_CWD = path.join(STATE_ROOT, ".empty");
const PENDING_TIMEOUT_MS = parseInt(process.env.CURSOR_AGENT_PENDING_TIMEOUT_MS || "600000", 10);
const SESSION_TTL_MS = parseInt(process.env.CURSOR_AGENT_SESSION_TTL_MS || "1800000", 10);
const MAX_SESSIONS = parseInt(process.env.CURSOR_AGENT_MAX_SESSIONS || "1000", 10);
const SSE_KEEPALIVE_MS = 15000;
// Per-session FIFO queue depth: concurrent NEW-USER turns on one session are serialized (not rejected);
// this bounds how many may wait behind the active turn before a last-resort 429 (Layer A diverts tool-less
// one-shots, so reaching this requires many genuine concurrent agentic turns on one conversation).
const MAX_QUEUE_DEPTH = parseInt(process.env.CURSOR_AGENT_MAX_QUEUE_DEPTH || "8", 10);
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

// toSdkImages maps the bridge image shape to the SDK's SDKImage ({data, mimeType}). The SDK requires
// BOTH fields for inline image data, so we validate each image and throw (failing the turn loudly)
// rather than silently sending a malformed image the SDK would reject or mis-render.
function toSdkImages(images) {
  if (!Array.isArray(images)) return [];
  return images.map((im, i) => {
    if (!im || typeof im.data !== "string" || !im.data || typeof im.mimeType !== "string" || !im.mimeType) {
      throw new Error(`[bridge] image[${i}] is missing required data/mimeType (the @cursor/sdk image shape is {data, mimeType})`);
    }
    return { data: im.data, mimeType: im.mimeType };
  });
}

// constraintInstructions turns the OpenAI-style enforced constraints the SDK has no first-class params
// for (response_format / stop / token limit / tool_choice required|specific) into a model instruction
// block appended to the user turn, so the Cursor agent honors what the request asked for.
function constraintInstructions({ toolChoice, responseFormat, stop, maxTokens } = {}) {
  const lines = [];
  const tc = toolChoice || "";
  if (tc === "required") {
    lines.push("You MUST call one of the available tools to fulfill this request; do not produce a final answer until you have called at least one tool.");
  } else if (tc.startsWith("specific:")) {
    const nm = tc.slice("specific:".length);
    lines.push(`You MUST call the tool named "${nm}" to fulfill this request, and you may call only that tool.`);
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
function effectiveAdvertise(advertise, toolChoice) {
  const adv = Array.isArray(advertise) ? advertise : [];
  const tc = toolChoice || "";
  if (tc === "none") return [];
  if (tc.startsWith("specific:")) {
    const nm = tc.slice("specific:".length);
    const only = adv.filter((t) => (t.toolName || t.name) === nm);
    return only.length ? only : adv;
  }
  return adv;
}

// ---- session correlation: the global executor learns its session via AsyncLocalStorage ----
const als = new AsyncLocalStorage(); // store = { session }
// The patch reads this to advertise the client's tools (incl MCPs) as mcp_tools, per-session.
globalThis.__CC_GET_ADVERTISE__ = () => {
  const st = als.getStore();
  if (!st || !st.session) { console.warn("[bridge] __CC_GET_ADVERTISE__: no ALS session context; advertising no tools"); return []; }
  return st.session.advertise || [];
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
// Native Cursor tool cases routed to CC. ccTool = generic name (CLIProxy maps to the client's exact tool
// + arg schema). buildResult/buildChunks turn CC's tool_result content into the Cursor result toJson shape.
const CC_CASES = {
  readArgs:        { ccTool: "read",  stream: false, buildResult: (c, s) => ({ success: { path: s && s.path, content: String(c ?? ""), totalLines: String(c ?? "").split("\n").length, fileSize: String(Buffer.byteLength(String(c ?? ""))), truncated: false, rangeApplied: false } }) },
  redactedReadArgs:{ ccTool: "read",  stream: false, buildResult: (c, s) => ({ success: { path: s && s.path, content: String(c ?? ""), totalLines: String(c ?? "").split("\n").length, fileSize: String(Buffer.byteLength(String(c ?? ""))), truncated: false, rangeApplied: false } }) },
  writeArgs:       { ccTool: "write", stream: false, buildResult: (c, s) => { const t = (s && s.fileText) || ""; const r = { success: { path: s && s.path, linesCreated: t.split("\n").length, fileSize: String(Buffer.byteLength(t)) } }; if (s && s.returnFileContentAfterWrite) r.success.fileContentAfterWrite = t; return r; } },
  deleteArgs:      { ccTool: "delete", stream: false, buildResult: (c, s) => ({ success: { path: s && s.path, deletedFile: true, fileSize: "0" } }) },
  shellArgs:       { ccTool: "shell", stream: false, buildResult: (c, s) => { const r = parseShellContent(c); return { success: { command: s && s.command, workingDirectory: "/workspace", exitCode: r.exitCode, stdout: r.stdout, stderr: r.stderr } }; } },
  shellStreamArgs: { ccTool: "shell", stream: true,  buildChunks: (c) => { const r = parseShellContent(c); const out = [{ stdout: { data: r.stdout } }]; if (r.stderr) out.push({ stderr: { data: r.stderr } }); out.push({ exit: { code: r.exitCode, cwd: "/workspace", aborted: r.aborted, localExecutionTimeMs: 1 } }); return out; } },
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

function ccArgsFor(cas, s) {
  switch (cas) {
    case "readArgs": case "redactedReadArgs": return { path: s && s.path, offset: s && s.offset, limit: s && s.limit };
    case "shellArgs": case "shellStreamArgs": return { command: s && s.command, cwd: (s && s.workingDirectory) || undefined };
    case "writeArgs": return { path: s && s.path, content: s && s.fileText };
    case "deleteArgs": return { path: s && s.path };
    default: return s;
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
  if (cas === "mcpArgs") {
    if (!store) return Promise.reject(new Error("[bridge] mcpArgs outside a session"));
    return store.session.dispatchMcp(s);
  }
  const spec = CC_CASES[cas];
  if (!spec || spec.stream || !spec.buildResult || !store) {
    return Promise.reject(new Error(`[bridge] tool '${cas}' not supported by the Claude Code bridge`));
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
    this.cursorKey = cursorKey || API_KEY; // the Cursor key whose platform this session runs on
    this.agent = null; this.agentPromise = null; this.run = null;
    this.activeRes = null; this.pending = new Map();
    this.turnBatch = []; this.flushTimer = null;
    this.delivered = new Set();   // tool ids the client has SEEN (sent in a turn_end) this logical run — so a
                                  // tool_results turn matches against what was actually delivered (Comment 2)
    this.undelivered = [];        // {id,name,input} of tools emitted with no open response (turn closed mid-burst);
                                  // delivered on the next /agent/turn so the client can answer them (Comment 1)
    this.turnToken = 0;           // increments per turn; flush is bound to a token
    this.settleTurn = null;
    this.streamedText = "";       // cumulative text streamed in the CURRENT run (reset per user turn)
    this.advertise = [];
    this.seeded = false;          // first user send done? system + history are prepended only on the first send
    this.clientEnv = null;        // client's real env (workspace/cwd/shell) for headless requestContext
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

  newPending(id, resolveWrap) {
    const timer = setTimeout(() => {
      const p = this.pending.get(id);
      if (p) { this.pending.delete(id); p.reject(new Error(`[bridge] tool ${id} abandoned after ${PENDING_TIMEOUT_MS}ms`)); }
    }, PENDING_TIMEOUT_MS);
    this.pending.set(id, { resolve: resolveWrap, reject: (err) => { clearTimeout(timer); resolveWrap.__reject(err); }, timer });
  }
  resolvePending(id, content) {
    const p = this.pending.get(id);
    if (!p) return false;
    clearTimeout(p.timer); this.pending.delete(id); p.resolve(content); return true;
  }

  dispatchUnary(cas, spec, s) {
    const id = ccToolId(s);
    return new Promise((resolve, reject) => {
      const wrap = (content) => { try { resolve({ __ccJson: spec.buildResult(content, s) }); } catch (err) { reject(err); } };
      wrap.__reject = reject;
      this.newPending(id, wrap);
      this.emitToolUse(id, spec.ccTool, ccArgsFor(cas, s));
    });
  }
  dispatchStream(cas, spec, s) {
    const id = ccToolId(s);
    const self = this;
    return (async function* () {
      const content = await new Promise((resolve, reject) => {
        const wrap = (c) => resolve(c); wrap.__reject = reject;
        self.newPending(id, wrap);
        self.emitToolUse(id, spec.ccTool, ccArgsFor(cas, s));
      });
      for (const chunk of spec.buildChunks(content)) yield { __ccJson: chunk };
    })();
  }
  // The model called one of the client's advertised tools (incl MCPs). Reconcile the (often paraphrased)
  // name against the advertised set, then route to CC. CC's text result becomes the McpResult content.
  dispatchMcp(s) {
    const id = ccToolId(s);
    const want = (s && (s.toolName || s.name)) || "";
    const ccName = this.reconcileToolName(want);
    const input = toPlainJson(s && s.args);
    if (!ccName) {
      const names = (this.advertise || []).map((t) => t.toolName || t.name).join(", ");
      return Promise.resolve({ __ccJson: { success: { isError: true, content: [{ text: { text: `Tool '${want}' is not available. Available tools: ${names || "(none)"}.` } }] } } });
    }
    return new Promise((resolve, reject) => {
      const wrap = (content) => resolve({ __ccJson: { success: { isError: false, content: [{ text: { text: typeof content === "string" ? content : JSON.stringify(content ?? "") } }] } } });
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
    if (adv.length === 1) return names[0];                              // only one tool: the model means it
    // token-boundary fuzzy: accept ONLY when exactly one advertised tool shares the want's last token.
    // (No substring includes() — that mis-routed e.g. "config" -> "reconfigure_database".)
    const tok = (s) => s.toLowerCase().split(/[-_.:/ ]+/).filter(Boolean);
    const tail = tok(lw).pop() || lw;
    const matches = names.filter((nm) => tok(nm).includes(tail));
    return matches.length === 1 ? matches[0] : null;                   // ambiguous/none -> typed isError
  }

  emitToolUse(id, name, input) {
    this.touch();
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
    this.flushTimer = setTimeout(() => { if (token === this.turnToken) this.pauseForTools(); }, 60);
  }
  // flushUndelivered delivers tools that were emitted while no response was open, on a later turn's OPEN
  // response, so the client finally sees them and can answer them. Emits one tool_use turn_end + settles.
  flushUndelivered() {
    if (!this.undelivered.length || !this.activeRes) return false;
    const batch = this.undelivered;
    this.undelivered = [];
    for (const t of batch) { this.delivered.add(t.id); this.sse({ type: "tool_call", id: t.id, name: t.name, input: t.input }); }
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
    dbg("onRunError", "session=" + this.id, (err && err.stack) || (err && err.message) || String(err));
    this.sse({ type: "turn_end", stop_reason: "error", error: (err && err.message) || String(err) });
    this.rejectAllPending("run errored");
    this.clearTurnState();
    this.settle();
    this.notifyLogicalDone(); // run terminated (error) -> admit the next queued new-user turn
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

async function ensureAgent(session, model) {
  if (session.agent) return session.agent;
  if (session.agentPromise) return session.agentPromise;          // guard TOCTOU
  session.agentPromise = (async () => {
    const platform = await getPlatform(session.cursorKey);
    const opts = { model: { id: model }, apiKey: session.cursorKey, local: { cwd: EMPTY_CWD } };
    dbg("ensureAgent resumeAgent", "session=" + session.id, "model=" + model);
    try {
      session.agent = await platform.resumeAgent(session.id, opts);       // cold / restart: resume by our stable id
    } catch (err) {
      // Only create-on-not-found. A transient resume error (model resolution / network) must NOT
      // fall through to createAgent (which PK-collides on an existing agent id) — rethrow so CLIProxy retries.
      const msg = (err && err.message) || String(err);
      dbg("ensureAgent resumeAgent FAILED", "session=" + session.id, msg);
      if (!/not found/i.test(msg)) { dbg("ensureAgent rethrow (not 'not found')", "session=" + session.id); throw err; }
      dbg("ensureAgent createAgent (was not found)", "session=" + session.id);
      session.agent = await platform.createAgent({ agentId: session.id, ...opts });
    }
    return session.agent;
  })();
  try { return await session.agentPromise; } finally { session.agentPromise = null; }
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
function dbg(...args) { try { writeSync(1, "[cct] " + args.map(safeJson).join(" ") + "\n"); } catch { /* never throw from logging */ } }

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
      // Forgiving degrade for an expired/unknown session (e.g. after a bridge restart or TTL eviction): the
      // tool_results reference pending calls a fresh session never issued, so we CANNOT resume them. Do NOT
      // 400 (clients retry-storm) and do NOT fake a re-seed (there is no matching pending). Emit a clean,
      // well-formed terminal so the client's stream ends gracefully; the conversation continues next turn.
      dbg("handleTurn tool_results -> unknown session (degrade to clean terminal)", sessionId);
      res.writeHead(200, SSE_HEADERS);
      try { res.write(`data: ${JSON.stringify({ type: "turn_end", stop_reason: "end_turn" })}\n\n`); res.write("data: [DONE]\n\n"); res.end(); } catch {}
      return;
    }
    // Comment 3: tool_results ingestion is NEVER 409'd. Resolving pending tool calls is just promise
    // resolution — safe regardless of any open response. Only the model-output STREAM is single-owner: if a
    // continuation response is already streaming this session's run (concurrent/incremental tool_results),
    // resolve the provided ids into the live run and return a short successful ack on THIS response, leaving
    // the model output on the existing activeRes. Otherwise (the normal case) drive the continuation here.
    if (session.activeRes) {
      res.writeHead(200, SSE_HEADERS);
      let matched = 0;
      for (const tr of input.results || []) { if (session.resolvePending(tr.toolCallId, tr.content)) matched++; }
      dbg("handleTurn tool_results CONCURRENT ack", sessionId, "matched=" + matched, "of=" + ((input.results || []).length));
      try { res.write(`data: ${JSON.stringify({ type: "turn_end", stop_reason: "end_turn" })}\n\n`); res.write("data: [DONE]\n\n"); res.end(); } catch {}
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
  const onWaitClose = () => { canceled = true; };
  res.on("close", onWaitClose);

  const prev = session.tail;
  let releaseNext;
  session.tail = new Promise((r) => { releaseNext = r; });

  prev.then(async () => {
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

  try {
    dbg("runTurn START", "session=" + session.id, "inputType=" + input.type, "turnToken=" + session.turnToken);
    if (input.type === "tool_results") {
      // Comment 2: match each result idempotently against pending; resolve what is provided and leave the
      // rest pending WITHOUT erroring (the client may answer incrementally / across requests). A re-sent
      // already-resolved id (or an id we delivered earlier) is a benign ack, never a fatal error. Only an id
      // we NEVER emitted to the client AND that isn't pending is "unknown".
      let matched = 0;
      const unknown = [];
      for (const tr of input.results || []) {
        if (session.resolvePending(tr.toolCallId, tr.content)) matched++;
        else if (!session.delivered.has(tr.toolCallId)) unknown.push(tr.toolCallId);
      }
      dbg("runTurn tool_results", "session=" + session.id, "matched=" + matched, "of=" + ((input.results || []).length),
        "pending=" + session.pending.size, "undelivered=" + session.undelivered.length, "unknown=" + safeJson(unknown));
      if (session.flushUndelivered()) {
        // Tools the SDK emitted after the prior turn closed (mid-burst) are now delivered as THIS turn's
        // tool_use batch (Comment 1) so the client can answer them; the run stays paused awaiting them.
      } else if (session.pending.size > 0) {
        // Some delivered tools remain unanswered (true incremental answer). The run is still blocked, so it
        // will neither stream nor complete this turn. Don't error and don't hang: settle a benign empty turn;
        // the run stays paused (bounded by PENDING_TIMEOUT_MS) and the client may answer the rest next.
        session.sse({ type: "turn_end", stop_reason: "end_turn" });
        session.settle();
      } else if (matched === 0) {
        // Nothing matched and nothing pending: a stale/duplicate ack (e.g. a client retry of an already-
        // resolved id). Acknowledge cleanly rather than erroring (Comment 2 idempotency) — this is what
        // breaks the old retry storm. When matched>0 and pending==0, the run resumes and streams below.
        session.sse({ type: "turn_end", stop_reason: "end_turn" });
        session.settle();
      }
    } else if (session.run) {
      // Re-entrancy guard: a new user turn while a run is still in flight (paused awaiting tools)
      // would spawn a second concurrent SDK run and orphan the first. CLIProxy should serialize
      // turns per sessionId; reject here as a backstop.
      dbg("runTurn RE-ENTRANT new user turn while run in flight -> reject", "session=" + session.id);
      session.sse({ type: "turn_end", stop_reason: "error", error: "a turn is already in progress for this session" });
      settleOnce();
    } else {
      session.streamedText = "";   // reset per user turn (NOT across tool-result continuations within a run)
      session.done = false;
      const agent = await ensureAgent(session, model);
      // ensureAgent's resume/create is a network round-trip; if the client disconnected during it, onClose
      // already settled+cancelled this turn. Bail BEFORE agent.send so we don't spawn an orphan run that
      // pins the FIFO slot and blocks eviction until the abandonment watchdog.
      if (settled) return;
      // First send for this session: prepend the system prompt + prior history so the SDK session starts
      // with full context. Later sends are just the new user text (the SDK holds the running conversation).
      let text = input.text || "";
      if (!session.seeded) {
        const parts = [];
        if (input.system) parts.push(input.system);
        if (input.history) parts.push("Previous conversation:\n" + input.history);
        if (text) parts.push(text);
        text = parts.join("\n\n");
        session.seeded = true;
      }
      // Enforced per-turn constraints (response_format / stop / token limit / tool_choice) as instructions.
      const ci = constraintInstructions(constraints);
      if (ci) text = text ? text + "\n\n" + ci : ci;
      // Build the message first (toSdkImages may throw on a malformed image) BEFORE gating advertisement,
      // so a bad image never leaves session.advertise in the restricted state.
      const msg = (Array.isArray(input.images) && input.images.length)
        ? { text, images: toSdkImages(input.images) }
        : text;
      // tool_choice gates what tools the model SEES this turn (none -> none; specific -> just that one).
      // Restore the full advertised set right after send: the run-request advertisement is built during
      // send, and reconcileToolName still resolves any tool the model calls against the full set.
      const savedAdvertise = session.advertise;
      session.advertise = effectiveAdvertise(session.advertise, constraints && constraints.toolChoice);
      dbg("runTurn NEW-TURN -> agent.send", "session=" + session.id, "seeded(after)=" + session.seeded,
        "msgTextLen=" + (typeof msg === "string" ? msg.length : (msg.text || "").length),
        "images=" + (Array.isArray(input.images) ? input.images.length : 0), "effAdvertise=" + session.advertise.length, "model=" + model);
      const ep = ++session.runEpoch; // this run's epoch; its completion callback must ignore a result if cancel() (or a later run) advanced it
      await als.run({ session }, async () => {
        try {
          session.run = await agent.send(msg, streamCallbacks(session));
        } catch (sendErr) {
          dbg("runTurn agent.send THREW", "session=" + session.id, (sendErr && sendErr.stack) || (sendErr && sendErr.message) || String(sendErr));
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
    let raw = ""; for await (const c of req) raw += c;
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

export { CC_CASES, headlessRequestContext, Session, reconcileExport, toSdkImages, constraintInstructions, effectiveAdvertise, parseShellContent, ccToolId, authorizeRequest, authorizeRequestWith, platformHasSession, keyHash, loadSdk, selfTestNativeUnreachable, selfTestBundleSeam };
function reconcileExport(advertise, want) { const s = new Session("x"); s.advertise = advertise; return s.reconcileToolName(want); }
