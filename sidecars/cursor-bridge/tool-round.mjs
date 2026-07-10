import {
  closeSync,
  existsSync,
  fsyncSync,
  linkSync,
  mkdirSync,
  openSync,
  readdirSync,
  readFileSync,
  renameSync,
  rmSync,
  statSync,
  writeFileSync,
  writeSync,
} from "node:fs";
import {
  createHash,
  createHmac,
  randomBytes,
  randomUUID,
  timingSafeEqual,
} from "node:crypto";
import path from "node:path";

export const ROUND_JOURNAL_VERSION = 1;
export const ROUTING_TOKEN_VERSION = "cct1";
export const TOOL_RESULT_CANONICAL_VERSION = 2;

export const DeferredInputState = Object.freeze({
  QUEUED: "QUEUED",
  DELIVERING: "DELIVERING",
  DELIVERED: "DELIVERED",
  SUPERSEDED: "SUPERSEDED",
});

// A continuation journal is rewritten atomically on each transition. Keep the
// mutable user-intent portion small and store the large replay context once per
// round; otherwise a retry loop multiplies a 500 KiB history into an ever
// larger fsync on every request.
export const MAX_DEFERRED_INPUT_RECORDS = 64;
export const MAX_DEFERRED_INTENT_BYTES = 1 << 20;
export const MAX_DEFERRED_CONTEXT_BYTES = 2 << 20;

export const RoundState = Object.freeze({
  COLLECTING: "COLLECTING",
  AWAITING_RESULTS: "AWAITING_RESULTS",
  APPLYING_RESULTS: "APPLYING_RESULTS",
  RESUMING: "RESUMING",
  TERMINAL: "TERMINAL",
});

export const TerminalReason = Object.freeze({
  COMPLETED: "completed",
  CLIENT_CANCELLED: "client_cancelled",
  INTERRUPTED: "interrupted",
  RUN_ERROR: "run_error",
  TRANSPORT_ERROR: "transport_error",
  PENDING_TIMEOUT: "pending_timeout",
  SESSION_EVICTED: "session_evicted",
  RESTART_LOST: "restart_lost",
  ROUND_LOST: "round_lost",
  LOOP_BOUND: "loop_bound",
  SHUTDOWN: "shutdown",
});

export const CallState = Object.freeze({
  REGISTERED: "REGISTERED",
  HANDED_TO_TRANSPORT: "HANDED_TO_TRANSPORT",
  RESULT_RECEIVED: "RESULT_RECEIVED",
  CALLBACK_APPLIED: "CALLBACK_APPLIED",
  TERMINAL: "TERMINAL",
});

export class ToolRoundError extends Error {
  constructor(code, message, status = 409, details = null) {
    super(message);
    this.name = "ToolRoundError";
    this.code = code;
    this.status = status;
    this.details = details;
  }
}

function jsonValue(value, at = "$") {
  if (value === null || typeof value === "string" || typeof value === "boolean") return value;
  if (typeof value === "number") {
    if (!Number.isFinite(value)) throw new ToolRoundError("non_json_result", `non-finite number at ${at}`, 422);
    return value;
  }
  if (Array.isArray(value)) return value.map((item, index) => jsonValue(item, `${at}[${index}]`));
  if (typeof value === "object") {
    const entries = [];
    for (const key of Object.keys(value).sort()) {
      if (value[key] === undefined) throw new ToolRoundError("non_json_result", `undefined value at ${at}.${key}`, 422);
      entries.push([key, jsonValue(value[key], `${at}.${key}`)]);
    }
    // Object.fromEntries defines own data properties even for `__proto__`;
    // assignment into `{}` would invoke the legacy prototype setter, drop the
    // JSON member, and make distinct durable results hash identically.
    return Object.fromEntries(entries);
  }
  throw new ToolRoundError("non_json_result", `unsupported ${typeof value} at ${at}`, 422);
}

export function canonicalJSONString(value) {
  return JSON.stringify(jsonValue(value));
}

const own = (value, key) => !!value && Object.prototype.hasOwnProperty.call(value, key);

function strictBooleanAlias(value, camel, snake, label) {
  const camelPresent = own(value, camel);
  const snakePresent = own(value, snake);
  const camelValue = camelPresent ? value[camel] : undefined;
  const snakeValue = snakePresent ? value[snake] : undefined;
  if (camelPresent && typeof camelValue !== "boolean") {
    throw new ToolRoundError("invalid_tool_result", `${label}.${camel} must be a boolean`, 422);
  }
  if (snakePresent && typeof snakeValue !== "boolean") {
    throw new ToolRoundError("invalid_tool_result", `${label}.${snake} must be a boolean`, 422);
  }
  if (camelPresent && snakePresent && camelValue !== snakeValue) {
    throw new ToolRoundError("invalid_tool_result", `${label}.${camel} conflicts with ${label}.${snake}`, 422);
  }
  return camelPresent ? camelValue : snakePresent ? snakeValue : false;
}

function parseDataURL(url) {
  if (typeof url !== "string" || !url.startsWith("data:")) return null;
  const comma = url.indexOf(",");
  if (comma <= 5) return null;
  const metadata = url.slice(5, comma);
  const parts = metadata.split(";");
  const mimeType = parts[0];
  if (!parts.slice(1).some((part) => part.toLowerCase() === "base64")) return null;
  const data = url.slice(comma + 1);
  if (!mimeType || !data) return null;
  return { data, mimeType };
}

function canonicalMimeType(value, at) {
  if (typeof value !== "string") {
    throw new ToolRoundError("invalid_tool_result", `image MIME type at ${at} must be a string`, 422);
  }
  const token = value.split(";", 1)[0].trim().toLowerCase();
  if (!/^[a-z0-9!#$&^_.+-]+\/[a-z0-9!#$&^_.+-]+$/.test(token)) {
    throw new ToolRoundError("invalid_tool_result", `image MIME type at ${at} is malformed`, 422);
  }
  return token;
}

function canonicalBase64(value, at) {
  if (typeof value !== "string") {
    throw new ToolRoundError("invalid_tool_result", `image base64 data at ${at} must be a string`, 422);
  }
  const compact = value.replace(/[\t\n\r ]/g, "");
  if (!compact || !/^[A-Za-z0-9+/]*={0,2}$/.test(compact) || compact.length % 4 === 1) {
    throw new ToolRoundError("invalid_tool_result", `image base64 data at ${at} is malformed`, 422);
  }
  const unpadded = compact.replace(/=+$/, "");
  const padded = unpadded + "=".repeat((4 - (unpadded.length % 4)) % 4);
  const decoded = Buffer.from(padded, "base64");
  const canonical = decoded.toString("base64");
  if (canonical.replace(/=+$/, "") !== unpadded) {
    throw new ToolRoundError("invalid_tool_result", `image base64 data at ${at} is non-canonical`, 422);
  }
  return canonical;
}

function canonicalImage(value, at) {
  const image = value && value.image && typeof value.image === "object" ? value.image : value;
  const source = image && image.source && typeof image.source === "object" ? image.source : null;
  const imageURL = image && image.image_url;
  let url = image && typeof image.url === "string" ? image.url : "";
  if (!url && typeof imageURL === "string") url = imageURL;
  if (!url && imageURL && typeof imageURL.url === "string") url = imageURL.url;
  if (!url && source && typeof source.url === "string") url = source.url;
  const dataURL = parseDataURL(url);
  if (dataURL) {
    return {
      type: "image",
      source: {
        kind: "base64",
        data: canonicalBase64(dataURL.data, `${at}.data`),
        mimeType: canonicalMimeType(dataURL.mimeType, `${at}.mimeType`),
      },
    };
  }
  if (url.startsWith("data:")) {
    throw new ToolRoundError("invalid_tool_result", `malformed base64 data URL at ${at}`, 422);
  }
  if (url) {
    const out = { kind: "url", url };
    const mimeType = (image && (image.mimeType || image.media_type || image.mediaType))
      || (source && (source.mimeType || source.media_type || source.mediaType));
    const detail = (imageURL && typeof imageURL === "object" && imageURL.detail)
      || (image && image.detail)
      || (source && source.detail);
    if (typeof mimeType === "string" && mimeType) out.mimeType = canonicalMimeType(mimeType, `${at}.mimeType`);
    if (typeof detail === "string" && detail) out.detail = detail;
    return { type: "image", source: out };
  }
  const data = (image && image.data) || (source && source.data);
  const mimeType = (image && (image.mimeType || image.media_type || image.mediaType))
    || (source && (source.mimeType || source.media_type || source.mediaType));
  if (typeof data === "string" && data && typeof mimeType === "string" && mimeType) {
    return {
      type: "image",
      source: {
        kind: "base64",
        data: canonicalBase64(data, `${at}.data`),
        mimeType: canonicalMimeType(mimeType, `${at}.mimeType`),
      },
    };
  }
  throw new ToolRoundError("invalid_tool_result", `malformed image content at ${at}`, 422);
}

function appendCanonicalBlock(blocks, block) {
  if (block.type === "text" && blocks.length && blocks.at(-1).type === "text") {
    const prior = blocks.at(-1).text;
    blocks.at(-1).text = `${prior}\n\n${block.text}`;
    return;
  }
  blocks.push(block);
}

function canonicalContentBlock(value, at) {
  if (typeof value === "string") return { type: "text", text: value };
  if (value && typeof value === "object") {
    if (value.type === "json" && own(value, "value")) {
      return { type: "json", value: jsonValue(value.value, `${at}.value`) };
    }
    if (value.type === "text" || own(value, "text")) {
      if (typeof value.text !== "string") {
        throw new ToolRoundError("invalid_tool_result", `text content at ${at} must be a string`, 422);
      }
      return { type: "text", text: value.text };
    }
    if (value.type === "image" || value.type === "image_url" || value.image || value.source
        || value.image_url || own(value, "data") || own(value, "url")) {
      return canonicalImage(value, at);
    }
  }
  return { type: "json", value: jsonValue(value, at) };
}

function canonicalContentBlocks(result) {
  const blocks = [];
  const hasBlocks = own(result, "contentBlocks");
  if (hasBlocks && !Array.isArray(result.contentBlocks)) {
    throw new ToolRoundError("invalid_tool_result", "contentBlocks must be an array", 422);
  }
  const content = hasBlocks
    ? result.contentBlocks
    : own(result, "content") && Array.isArray(result.content)
      ? result.content
      : [own(result, "content") ? result.content : ""];
  for (let index = 0; index < content.length; index++) {
    appendCanonicalBlock(blocks, canonicalContentBlock(content[index], `$.contentBlocks[${index}]`));
  }
  // contentBlocks is authoritative. `images` is the compatibility projection
  // of those same blocks and must not be counted a second time.
  if (!hasBlocks && Array.isArray(result && result.images)) {
    for (let index = 0; index < result.images.length; index++) {
      appendCanonicalBlock(blocks, canonicalImage(result.images[index], `$.images[${index}]`));
    }
  }
  return blocks;
}

function canonicalStructuredContent(result) {
  const explicitPresence = own(result, "structuredContentPresent");
  if (explicitPresence && typeof result.structuredContentPresent !== "boolean") {
    throw new ToolRoundError("invalid_tool_result", "structuredContentPresent must be a boolean", 422);
  }
  const camelPresent = own(result, "structuredContent");
  const snakePresent = own(result, "structured_content");
  if (camelPresent && snakePresent
      && canonicalJSONString(result.structuredContent) !== canonicalJSONString(result.structured_content)) {
    throw new ToolRoundError("invalid_tool_result", "structuredContent conflicts with structured_content", 422);
  }
  const present = explicitPresence
    ? result.structuredContentPresent
    : camelPresent || snakePresent;
  if (!present) return { present: false };
  const value = camelPresent ? result.structuredContent : snakePresent ? result.structured_content : null;
  return { present: true, value: jsonValue(value, "$.structuredContent") };
}

// V2 defines result identity by semantic content, not by the incidental
// OpenAI/Anthropic envelope that carried it. Adjacent text blocks coalesce with
// the same paragraph separator, image order stays significant, error aliases
// are strict, and structured-content presence is retained separately from null.
export function canonicalResult(result) {
  const value = result && typeof result === "object" ? result : {};
  if (typeof value.contractError === "string" && value.contractError) {
    throw new ToolRoundError("invalid_tool_result", value.contractError, 422);
  }
  return {
    canonicalVersion: TOOL_RESULT_CANONICAL_VERSION,
    blocks: canonicalContentBlocks(value),
    isError: strictBooleanAlias(value, "isError", "is_error", "toolResult"),
    structuredContent: canonicalStructuredContent(value),
    transformPolicy: "tool-result-v2",
    truncationPolicy: "live-result-v1",
  };
}

export function legacyCanonicalResult(result) {
  return {
    content: result && own(result, "content") ? result.content : "",
    images: result && Array.isArray(result.images) ? result.images : [],
    isError: !!(result && result.isError === true),
    structuredContent: result && own(result, "structuredContent") ? result.structuredContent : null,
  };
}

export function hashLegacyClientToolResult(result) {
  return createHash("sha256").update(canonicalJSONString(legacyCanonicalResult(result))).digest("hex");
}

export function hashClientToolResult(result) {
  return createHash("sha256").update(canonicalJSONString(canonicalResult(result))).digest("hex");
}

function hashCanonicalResult(semantic) {
  return createHash("sha256").update(canonicalJSONString(semantic)).digest("hex");
}

function deliveryResultFromSemantic(semantic) {
  const projectedImages = semantic.blocks
    .filter((block) => block.type === "image")
    .map((block) => block.source.kind === "url"
      ? {
        url: block.source.url,
        ...(block.source.mimeType ? { mimeType: block.source.mimeType } : {}),
        ...(block.source.detail ? { detail: block.source.detail } : {}),
      }
      : { data: block.source.data, mimeType: block.source.mimeType });
  const nonImageBlocks = semantic.blocks.filter((block) => block.type !== "image");
  const content = nonImageBlocks.length === 1 && nonImageBlocks[0].type === "json"
    ? nonImageBlocks[0].value
    : nonImageBlocks.map((block) => block.type === "text" ? block.text : canonicalJSONString(block.value)).join("\n\n");
  return {
    content,
    contentBlocks: jsonValue(semantic.blocks, "$.contentBlocks"),
    images: projectedImages,
    isError: semantic.isError,
    structuredContent: semantic.structuredContent.present ? semantic.structuredContent.value : null,
    structuredContentPresent: semantic.structuredContent.present,
  };
}

function deliveryResult(result) {
  const value = result && typeof result === "object" ? result : {};
  return deliveryResultFromSemantic(canonicalResult(value));
}

function canonicalDeferredInput(input) {
  const value = input && typeof input === "object" ? input : {};
  const text = typeof value.userText === "string"
    ? value.userText
    : typeof value.text === "string" ? value.text : "";
  const images = Array.isArray(value.images) ? jsonValue(value.images, "$.images") : [];
  return { text, images };
}

function canonicalDeferredContext(input) {
  const value = input && typeof input === "object" ? input : {};
  return {
    system: typeof value.system === "string" ? value.system : "",
    history: typeof value.history === "string" ? value.history : "",
    historyFingerprint: typeof value.historyFingerprint === "string" ? value.historyFingerprint : "",
  };
}

function jsonBytes(value) {
  return Buffer.byteLength(canonicalJSONString(value), "utf8");
}

function validateDeferredSizes(intent, context) {
  const intentBytes = jsonBytes(intent);
  if (intentBytes > MAX_DEFERRED_INTENT_BYTES) {
    throw new ToolRoundError(
      "deferred_input_too_large",
      `deferred user input exceeds ${MAX_DEFERRED_INTENT_BYTES} bytes`,
      413,
    );
  }
  const contextBytes = jsonBytes(context);
  if (contextBytes > MAX_DEFERRED_CONTEXT_BYTES) {
    throw new ToolRoundError(
      "deferred_context_too_large",
      `deferred recovery context exceeds ${MAX_DEFERRED_CONTEXT_BYTES} bytes`,
      413,
    );
  }
  return { intentBytes, contextBytes };
}

function compactDeferredState(record) {
  const source = Array.isArray(record.deferredInputs) ? record.deferredInputs : [];
  let recoveryContext = record.recoveryContext && typeof record.recoveryContext === "object"
    ? canonicalDeferredContext(record.recoveryContext)
    : null;
  // Legacy records duplicated system/history inside every entry. The newest
  // copy is the authoritative stateless request snapshot.
  if (!recoveryContext) {
    for (let index = source.length - 1; index >= 0; index--) {
      const input = source[index] && source[index].input;
      if (input && typeof input === "object") {
        recoveryContext = canonicalDeferredContext(input);
        break;
      }
    }
  }
  recoveryContext ||= canonicalDeferredContext(null);

  const normalized = source.map((entry) => {
    const state = Object.values(DeferredInputState).includes(entry && entry.state)
      ? entry.state
      : DeferredInputState.QUEUED;
    const actionable = state === DeferredInputState.QUEUED || state === DeferredInputState.DELIVERING;
    return {
      ...entry,
      state,
      input: actionable ? canonicalDeferredInput(entry && entry.input) : null,
    };
  });

  // Each HTTP request is a complete stateless snapshot. A newer QUEUED
  // snapshot supersedes older unsent snapshots; retaining all of them both
  // replays stale intent and creates an unbounded journal. Preserve compact
  // tombstones so a delayed retry of an older id is acknowledged as stale.
  const newestQueued = [...normalized].reverse().find((entry) => entry.state === DeferredInputState.QUEUED) || null;
  if (newestQueued) {
    for (const entry of normalized) {
      if (entry !== newestQueued && entry.state === DeferredInputState.QUEUED) {
        entry.state = DeferredInputState.SUPERSEDED;
        entry.input = null;
        entry.supersededBy = newestQueued.clientMessageId;
        entry.supersededAt ||= newestQueued.receivedAt || record.updatedAt || Date.now();
      }
    }
  }

  const actionable = normalized.filter((entry) => entry.state === DeferredInputState.QUEUED
    || entry.state === DeferredInputState.DELIVERING);
  const tombstones = normalized.filter((entry) => entry.state !== DeferredInputState.QUEUED
    && entry.state !== DeferredInputState.DELIVERING).slice(-MAX_DEFERRED_INPUT_RECORDS);
  return {
    deferredInputs: [...tombstones, ...actionable]
      .sort((left, right) => (left.receivedAt || 0) - (right.receivedAt || 0)),
    recoveryContext,
  };
}

function hashDeferredInput(input) {
  const payload = canonicalDeferredInput(input);
  // The client message id identifies immutable user intent. Recovery context
  // (history compaction, system refresh, and interrupt routing) may change on
  // a byte-equivalent retry and must not turn that retry into a 409 conflict.
  return createHash("sha256").update(canonicalJSONString(payload)).digest("hex");
}

function rekeyConflictingClientMessageId(requestedId, hash, existingEntries) {
  const digest = createHash("sha256")
    .update("cursor-client-message-conflict-v1\0")
    .update(requestedId)
    .update("\0")
    .update(hash)
    .digest("base64url");
  const base = `ccm2_rekey_${digest}`;
  let candidate = base;
  // Do not rely on hash-collision impossibility when reading durable state.
  // A deterministic suffix finds the first compatible slot, so every retry
  // of the same conflicting intent resolves to the same durable message.
  for (let suffix = 0; suffix <= existingEntries.length; suffix++) {
    const existing = existingEntries.find((item) => item.clientMessageId === candidate);
    if (!existing || existing.hash === hash) return { candidate, existing };
    candidate = `${base}_${suffix + 1}`;
  }
  throw new ToolRoundError("journal_corrupt", "unable to allocate a deterministic client-message conflict route", 500);
}

function legacyReceiptSemanticInput(result) {
  if (!result || typeof result !== "object") return result;
  // V1 always materialized absent structured content as null, so an old
  // journal cannot distinguish absent from explicitly-null. Prefer absence for
  // migration compatibility; V2 records the presence bit and is unambiguous.
  if (!own(result, "structuredContentPresent") && result.structuredContent === null) {
    return { ...result, structuredContentPresent: false };
  }
  return result;
}

function base64url(input) {
  return Buffer.from(input).toString("base64url");
}

function decodeBase64url(value, label) {
  if (!/^[A-Za-z0-9_-]+$/.test(value)) throw new ToolRoundError("invalid_tool_call_id", `${label} is not base64url`, 400);
  try {
    const decoded = Buffer.from(value, "base64url");
    if (base64url(decoded) !== value) throw new Error("non-canonical base64url");
    return decoded;
  }
  catch { throw new ToolRoundError("invalid_tool_call_id", `${label} is malformed`, 400); }
}

export class RoutingTokenCodec {
  constructor(secret) {
    this.secret = Buffer.from(secret || []);
    if (this.secret.length < 32) throw new ToolRoundError("routing_key_invalid", "routing-token key must be at least 32 bytes", 500);
  }

  newRoute() {
    return base64url(randomBytes(16));
  }

  signature(route, ordinalText) {
    return createHmac("sha256", this.secret)
      .update(`${ROUTING_TOKEN_VERSION}\0${route}\0${ordinalText}`)
      .digest()
      .subarray(0, 16);
  }

  issue(route, ordinal) {
    if (!/^[A-Za-z0-9_-]{20,30}$/.test(route)) throw new ToolRoundError("route_invalid", "round route is malformed", 500);
    if (!Number.isSafeInteger(ordinal) || ordinal < 0 || ordinal > 0x7fffffff) {
      throw new ToolRoundError("ordinal_invalid", "tool-call ordinal is out of range", 500);
    }
    const ordinalText = ordinal.toString(36);
    return `${ROUTING_TOKEN_VERSION}_${route}_${ordinalText}_${base64url(this.signature(route, ordinalText))}`;
  }

  parse(token) {
    const match = /^([a-z0-9]+)_([A-Za-z0-9_-]{20,30})_([0-9a-z]{1,7})_([A-Za-z0-9_-]{22})$/.exec(String(token || ""));
    if (!match || match[1] !== ROUTING_TOKEN_VERSION) {
      throw new ToolRoundError("invalid_tool_call_id", "tool-call id is not a supported signed routing token", 400);
    }
    const [, , route, ordinalText, signatureText] = match;
    const ordinal = Number.parseInt(ordinalText, 36);
    if (!Number.isSafeInteger(ordinal) || ordinal < 0 || ordinal > 0x7fffffff || ordinal.toString(36) !== ordinalText) {
      throw new ToolRoundError("invalid_tool_call_id", "tool-call id has an invalid ordinal", 400);
    }
    const supplied = decodeBase64url(signatureText, "tool-call signature");
    const expected = this.signature(route, ordinalText);
    if (supplied.length !== expected.length || !timingSafeEqual(supplied, expected)) {
      throw new ToolRoundError("invalid_tool_call_id", "tool-call id signature is invalid", 400);
    }
    return { route, ordinal, token: String(token) };
  }
}

function fsyncDirectory(dir) {
  const fd = openSync(dir, "r");
  try { fsyncSync(fd); }
  finally { closeSync(fd); }
}

function atomicWrite(file, bytes, mode = 0o600) {
  const dir = path.dirname(file);
  mkdirSync(dir, { recursive: true, mode: 0o700 });
  const tmp = path.join(dir, `.${path.basename(file)}.${process.pid}.${randomUUID()}.tmp`);
  const fd = openSync(tmp, "wx", mode);
  try {
    writeSync(fd, bytes);
    fsyncSync(fd);
  } finally {
    closeSync(fd);
  }
  try {
    renameSync(tmp, file);
    fsyncDirectory(dir);
  } catch (error) {
    try { rmSync(tmp, { force: true }); } catch {}
    throw error;
  }
}

export function loadOrCreateRoutingKey(stateRoot) {
  mkdirSync(stateRoot, { recursive: true, mode: 0o700 });
  const keyPath = path.join(stateRoot, ".client-tool-routing.key");
  if (!existsSync(keyPath)) {
    const candidate = path.join(stateRoot, `.client-tool-routing.${process.pid}.${randomUUID()}.tmp`);
    const fd = openSync(candidate, "wx", 0o600);
    try {
      const generated = randomBytes(32);
      writeSync(fd, generated);
      fsyncSync(fd);
    } finally {
      closeSync(fd);
    }
    try {
      // link(2) publishes a fully-written candidate without ever replacing a
      // winner from a concurrent bridge start. Readers can therefore observe
      // either no key or the complete 32-byte key, never a partially-written
      // key file.
      linkSync(candidate, keyPath);
      fsyncDirectory(stateRoot);
    } catch (error) {
      if (!error || error.code !== "EEXIST") throw error;
    } finally {
      try { rmSync(candidate, { force: true }); } catch {}
    }
  }
  const key = readFileSync(keyPath);
  if (key.length !== 32) throw new ToolRoundError("routing_key_invalid", `routing key ${keyPath} must contain exactly 32 bytes`, 500);
  return key;
}

function validRoute(route) {
  return /^[A-Za-z0-9_-]{20,30}$/.test(String(route || ""));
}

export class RoundJournal {
  constructor(stateRoot, { now = () => Date.now(), staleLockMs = 60_000 } = {}) {
    this.stateRoot = path.resolve(stateRoot);
    this.dir = path.join(this.stateRoot, "client-tool-rounds");
    this.now = now;
    this.staleLockMs = staleLockMs;
    mkdirSync(this.dir, { recursive: true, mode: 0o700 });
  }

  file(route) {
    if (!validRoute(route)) throw new ToolRoundError("route_invalid", "round route is malformed", 400);
    return path.join(this.dir, `${route}.json`);
  }

  lockDir(route) {
    return path.join(this.dir, `${route}.lock`);
  }

  acquire(route) {
    const lock = this.lockDir(route);
    try {
      mkdirSync(lock, { mode: 0o700 });
      writeFileSync(path.join(lock, "owner.json"), JSON.stringify({ pid: process.pid, createdAt: this.now() }), { mode: 0o600 });
      return () => { try { rmSync(lock, { recursive: true, force: true }); } catch {} };
    } catch (error) {
      if (!error || error.code !== "EEXIST") throw error;
      let age = 0;
      try { age = this.now() - statSync(lock).mtimeMs; } catch {}
      if (age > this.staleLockMs) {
        try { rmSync(lock, { recursive: true, force: true }); }
        catch (removeError) { throw new ToolRoundError("round_busy", `round ${route} has an unrecoverable stale lock: ${removeError.message}`, 503); }
        return this.acquire(route);
      }
      throw new ToolRoundError("round_busy", `round ${route} is being updated`, 503);
    }
  }

  read(route) {
    const file = this.file(route);
    let parsed;
    try { parsed = JSON.parse(readFileSync(file, "utf8")); }
    catch (error) {
      if (error && error.code === "ENOENT") return null;
      throw new ToolRoundError("journal_corrupt", `cannot read round journal ${route}: ${error.message}`, 500);
    }
    this.validate(parsed, route);
    return parsed;
  }

  validate(record, route = record && record.route) {
    if (!record || record.version !== ROUND_JOURNAL_VERSION || record.route !== route || !validRoute(record.route)) {
      throw new ToolRoundError("journal_corrupt", `round journal ${route || "<unknown>"} has an invalid envelope`, 500);
    }
    if (!Number.isSafeInteger(record.revision) || record.revision < 1 || !Array.isArray(record.calls)) {
      throw new ToolRoundError("journal_corrupt", `round journal ${route} has an invalid revision or call list`, 500);
    }
    return record;
  }

  save(next, expectedRevision = 0) {
    const release = this.acquire(next.route);
    try {
      const current = this.read(next.route);
      const currentRevision = current ? current.revision : 0;
      if (currentRevision !== expectedRevision) {
        throw new ToolRoundError(
          "journal_revision_conflict",
          `round ${next.route} revision changed from ${expectedRevision} to ${currentRevision}`,
          409,
          { expectedRevision, currentRevision },
        );
      }
      const saved = jsonValue({ ...next, version: ROUND_JOURNAL_VERSION, revision: currentRevision + 1, updatedAt: this.now() });
      atomicWrite(this.file(next.route), `${JSON.stringify(saved)}\n`);
      return saved;
    } finally {
      release();
    }
  }

  records() {
    const records = [];
    for (const entry of readdirSync(this.dir, { withFileTypes: true })) {
      if (!entry.isFile() || !entry.name.endsWith(".json")) continue;
      const route = entry.name.slice(0, -5);
      if (!validRoute(route)) continue;
      records.push(this.read(route));
    }
    return records;
  }

  // Bound only terminal receipts. Open/awaiting/applying rounds are never
  // eligible, even when they are old or the terminal cap is exceeded.
  cleanupTerminal({ ttlMs = 7 * 24 * 60 * 60 * 1000, maxTerminal = 10_000 } = {}) {
    if (!Number.isSafeInteger(ttlMs) || ttlMs < 0 || !Number.isSafeInteger(maxTerminal) || maxTerminal < 0) {
      throw new ToolRoundError("journal_retention_invalid", "terminal journal retention values must be non-negative integers", 500);
    }
    const terminal = [];
    for (const entry of readdirSync(this.dir, { withFileTypes: true })) {
      if (!entry.isFile() || !entry.name.endsWith(".json")) continue;
      const route = entry.name.slice(0, -5);
      if (!validRoute(route)) continue;
      const record = this.read(route);
      const unresolvedDeferred = Array.isArray(record?.deferredInputs)
        && record.deferredInputs.some((item) => item && (
          item.state === DeferredInputState.QUEUED || item.state === DeferredInputState.DELIVERING
        ));
      if (record?.state === RoundState.TERMINAL && !unresolvedDeferred) terminal.push(record);
    }
    terminal.sort((a, b) => (a.updatedAt || 0) - (b.updatedAt || 0));
    const cutoff = this.now() - ttlMs;
    const remove = new Set(terminal.filter((record) => (record.updatedAt || 0) < cutoff).map((record) => record.route));
    const retainedAfterAge = terminal.filter((record) => !remove.has(record.route));
    for (const record of retainedAfterAge.slice(0, Math.max(0, retainedAfterAge.length - maxTerminal))) remove.add(record.route);
    let removed = 0;
    for (const route of remove) {
      const release = this.acquire(route);
      try {
        const current = this.read(route);
        const unresolvedDeferred = Array.isArray(current?.deferredInputs)
          && current.deferredInputs.some((item) => item && (
            item.state === DeferredInputState.QUEUED || item.state === DeferredInputState.DELIVERING
          ));
        if (current?.state !== RoundState.TERMINAL || unresolvedDeferred) continue;
        rmSync(this.file(route), { force: true });
        fsyncDirectory(this.dir);
        removed++;
      } finally {
        release();
      }
    }
    return { removed, retained: terminal.length - removed };
  }
}

export function createRoundInfrastructure(stateRoot, options = {}) {
  const journal = new RoundJournal(stateRoot, options);
  const codec = new RoutingTokenCodec(options.secret || loadOrCreateRoutingKey(stateRoot));
  return { journal, codec };
}

// Verify every filesystem primitive the journal relies on before readiness.
export function probeStateRoot(stateRoot) {
  const root = path.resolve(stateRoot);
  const dir = path.join(root, ".client-tool-state-probe");
  mkdirSync(dir, { recursive: true, mode: 0o700 });
  const expected = randomBytes(32);
  const source = path.join(dir, `${process.pid}-${randomUUID()}.new`);
  const target = `${source}.committed`;
  let fd;
  try {
    fd = openSync(source, "wx", 0o600);
    writeSync(fd, expected);
    fsyncSync(fd);
    closeSync(fd);
    fd = undefined;
    renameSync(source, target);
    fsyncDirectory(dir);
    const observed = readFileSync(target);
    if (observed.length !== expected.length || !timingSafeEqual(observed, expected)) {
      throw new ToolRoundError("state_root_probe_failed", "STATE_ROOT read-after-rename did not preserve bytes", 500);
    }
    rmSync(target, { force: true });
    fsyncDirectory(dir);
    return { ok: true, stateRoot: root };
  } finally {
    if (fd !== undefined) closeSync(fd);
    try { rmSync(source, { force: true }); } catch {}
    try { rmSync(target, { force: true }); } catch {}
  }
}

function serializedCall(call) {
  return {
    callbackAppliedAt: call.callbackAppliedAt,
    handedAt: call.handedAt,
    input: call.input,
    descriptorFingerprint: call.descriptorFingerprint || null,
    inventoryEpoch: call.inventoryEpoch || null,
    name: call.name,
    ordinal: call.ordinal,
    rawIdHash: call.rawIdHash,
    receipt: call.receipt,
    resultHash: call.resultHash,
    resultHashVersion: call.resultHashVersion || null,
    semanticResultHash: call.semanticResultHash || null,
    source: call.source,
    state: call.state,
    terminalAt: call.terminalAt,
    wireId: call.wireId,
  };
}

function callFromRecord(saved) {
  return {
    ...saved,
    callback: null,
    newlyRegistered: false,
    timer: null,
    waiters: [],
  };
}

function allowedRoundTransition(from, to) {
  if (from === to) return true;
  const allowed = {
    [RoundState.COLLECTING]: new Set([RoundState.AWAITING_RESULTS, RoundState.TERMINAL]),
    [RoundState.AWAITING_RESULTS]: new Set([RoundState.APPLYING_RESULTS, RoundState.TERMINAL]),
    [RoundState.APPLYING_RESULTS]: new Set([RoundState.AWAITING_RESULTS, RoundState.RESUMING, RoundState.TERMINAL]),
    [RoundState.RESUMING]: new Set([RoundState.TERMINAL]),
    [RoundState.TERMINAL]: new Set(),
  };
  return !!(allowed[from] && allowed[from].has(to));
}

export class ToolRound {
  constructor({
    sessionId,
    agentId = sessionId,
    runEpoch = 0,
    roundSeq = 0,
    route = null,
    roundId = null,
    tenantFingerprint = "",
    model = "",
    journal = null,
    codec = null,
    clock = () => Date.now(),
    timers = { setTimeout, clearTimeout },
    terminalCallback = null,
    record = null,
  }) {
    this.clock = clock;
    this.timers = timers;
    this.journal = journal;
    this.codec = codec;
    this.terminalCallback = terminalCallback;
    this.callbacks = new Map();
    this.rawToCall = new Map();

    if (record) {
      if (journal) journal.validate(record);
      this.version = record.version;
      this.revision = record.revision;
      this.route = record.route;
      this.roundId = record.roundId || record.route;
      this.sessionId = record.sessionId;
      this.agentId = record.agentId;
      this.runEpoch = record.runEpoch;
      this.roundSeq = record.roundSeq;
      this.tenantFingerprint = record.tenantFingerprint || "";
      this.model = record.model || "";
      this.createdAt = record.createdAt;
      this.updatedAt = record.updatedAt;
      this.state = record.state;
      this.terminal = record.terminal || null;
      this.recovery = record.recovery || null;
      const compactedDeferred = compactDeferredState(record);
      this.deferredInputs = compactedDeferred.deferredInputs;
      this.recoveryContext = compactedDeferred.recoveryContext;
      this.calls = new Map(record.calls.map((saved) => [saved.wireId, callFromRecord(saved)]));
      for (const call of this.calls.values()) {
        if (call.rawIdHash) this.rawToCall.set(call.rawIdHash, call.wireId);
      }
      this.fifo = record.calls.slice().sort((a, b) => a.ordinal - b.ordinal).map((call) => call.wireId);
      this.outbound = this.fifo.filter((id) => this.calls.get(id).state === CallState.REGISTERED);
      this.batch = this.fifo.filter((id) => this.calls.get(id).state === CallState.HANDED_TO_TRANSPORT);
      return;
    }

    if (!sessionId) throw new ToolRoundError("session_required", "ToolRound requires a session id", 500);
    this.version = ROUND_JOURNAL_VERSION;
    this.revision = 0;
    this.route = route || (codec ? codec.newRoute() : base64url(randomBytes(16)));
    this.roundId = roundId || this.route;
    this.sessionId = sessionId;
    this.agentId = agentId || sessionId;
    this.runEpoch = runEpoch;
    this.roundSeq = roundSeq;
    this.tenantFingerprint = tenantFingerprint;
    this.model = model;
    this.createdAt = this.clock();
    this.updatedAt = this.createdAt;
    this.state = RoundState.COLLECTING;
    this.terminal = null;
    this.recovery = null;
    this.deferredInputs = [];
    this.recoveryContext = canonicalDeferredContext(null);
    this.calls = new Map();
    this.fifo = [];
    this.outbound = [];
    this.batch = [];
    this.persistRecord(this.toRecord());
  }

  static load(journal, codec, route, options = {}) {
    const record = journal.read(route);
    return record ? new ToolRound({ ...options, journal, codec, record }) : null;
  }

  get pending() { return this.callbacks; }
  get pendingCount() { return this.callbacks.size; }
  get isTerminal() { return this.state === RoundState.TERMINAL; }

  toRecord() {
    return {
      agentId: this.agentId,
      calls: this.fifo.map((id) => serializedCall(this.calls.get(id))),
      createdAt: this.createdAt,
      deferredInputs: this.deferredInputs.map((input) => ({ ...input })),
      recoveryContext: this.recoveryContext,
      model: this.model,
      recovery: this.recovery,
      revision: this.revision,
      roundId: this.roundId,
      roundSeq: this.roundSeq,
      route: this.route,
      runEpoch: this.runEpoch,
      sessionId: this.sessionId,
      state: this.state,
      tenantFingerprint: this.tenantFingerprint,
      terminal: this.terminal,
      updatedAt: this.updatedAt,
      version: ROUND_JOURNAL_VERSION,
    };
  }

  persistRecord(record) {
    if (!this.journal) {
      this.revision += 1;
      this.updatedAt = this.clock();
      return { ...record, revision: this.revision, updatedAt: this.updatedAt };
    }
    const saved = this.journal.save(record, this.revision);
    this.revision = saved.revision;
    this.updatedAt = saved.updatedAt;
    return saved;
  }

  nextWireId(ordinal) {
    return this.codec
      ? this.codec.issue(this.route, ordinal)
      : `${ROUTING_TOKEN_VERSION}_${this.route}_${ordinal.toString(36)}_${base64url(randomBytes(16))}`;
  }

  openCall({
    source = "sdk",
    rawToolCallId = null,
    name,
    input,
    callback = null,
    inventoryEpoch = null,
    descriptorFingerprint = null,
  }) {
    // The SDK may dispatch the tail of one parallel wave after the bridge has
    // already handed the first batch and entered AWAITING_RESULTS. Keep that
    // tail in this same durable round; Session will journal it for the next
    // response instead of writing a tool_call after turn_end.
    if (this.state !== RoundState.COLLECTING && this.state !== RoundState.AWAITING_RESULTS) {
      throw new ToolRoundError("round_not_collecting", `round ${this.route} cannot open calls while ${this.state}`, 409);
    }
    if (!name) throw new ToolRoundError("tool_name_required", "client tool name is required", 500);
    // Canonicalize once for both durable storage and duplicate comparison.
    // JSON transport intentionally drops undefined optional placeholders and
    // normalizes -0 exactly as the SDK wire does; comparing against the raw
    // pre-transport object caused false raw_tool_id conflicts.
    const encodedInput = JSON.stringify(input);
    const canonicalInput = encodedInput === undefined ? null : jsonValue(JSON.parse(encodedInput));
    const rawIdHash = rawToolCallId == null ? null : createHash("sha256").update(String(rawToolCallId)).digest("hex");
    if (rawIdHash && this.rawToCall.has(rawIdHash)) {
      const existing = this.calls.get(this.rawToCall.get(rawIdHash));
      const inputChanged = canonicalJSONString(existing.input) !== canonicalJSONString(canonicalInput);
      if (existing.name !== name || (inputChanged && existing.state !== CallState.REGISTERED)) {
        throw new ToolRoundError("raw_tool_id_conflict", "SDK reused a handed tool-call id with incompatible name or input", 409);
      }
      if (inputChanged) {
        // Incremental SDK assembly may invoke the dispatch seam more than once
        // for one raw id before any bytes are handed to the harness. The
        // registered journal entry is still private and may adopt the latest
        // complete arguments. Handoff freezes it permanently.
        const next = this.toRecord();
        const saved = next.calls.find((item) => item.wireId === existing.wireId);
        saved.input = canonicalInput;
        saved.inventoryEpoch = typeof inventoryEpoch === "string" ? inventoryEpoch : saved.inventoryEpoch;
        saved.descriptorFingerprint = typeof descriptorFingerprint === "string"
          ? descriptorFingerprint : saved.descriptorFingerprint;
        this.persistRecord(next);
        existing.input = canonicalInput;
        existing.inventoryEpoch = saved.inventoryEpoch;
        existing.descriptorFingerprint = saved.descriptorFingerprint;
      }
      existing.newlyRegistered = false;
      if (callback && existing.receipt) {
        callback.resolve(existing.receipt.result);
      } else if (callback && !existing.callback) {
        existing.callback = callback;
        this.callbacks.set(existing.wireId, existing);
      } else if (callback && callback !== existing.callback) {
        existing.waiters.push(callback);
      }
      return existing;
    }
    const ordinal = this.fifo.length;
    const call = {
      callback,
      callbackAppliedAt: null,
      descriptorFingerprint: typeof descriptorFingerprint === "string" ? descriptorFingerprint : null,
      handedAt: null,
      input: canonicalInput,
      inventoryEpoch: typeof inventoryEpoch === "string" ? inventoryEpoch : null,
      name: String(name),
      newlyRegistered: true,
      ordinal,
      rawIdHash,
      receipt: null,
      resultHash: null,
      resultHashVersion: null,
      semanticResultHash: null,
      source: String(source || "sdk"),
      state: CallState.REGISTERED,
      terminalAt: null,
      timer: null,
      waiters: [],
      wireId: this.nextWireId(ordinal),
    };
    const next = this.toRecord();
    next.calls.push(serializedCall(call));
    this.persistRecord(next);
    this.calls.set(call.wireId, call);
    this.fifo.push(call.wireId);
    this.outbound.push(call.wireId);
    if (rawIdHash) this.rawToCall.set(rawIdHash, call.wireId);
    if (callback) this.callbacks.set(call.wireId, call);
    return call;
  }

  attachCallback(wireId, callback) {
    const call = this.calls.get(wireId);
    if (!call) throw new ToolRoundError("unknown_tool_call_id", `round ${this.route} does not contain ${wireId}`, 410);
    if (callback && call.receipt) {
      callback.resolve(call.receipt.result);
      return call;
    }
    if (call.callback && call.callback !== callback) call.waiters.push(callback);
    else call.callback = callback;
    if (callback && call.state !== CallState.CALLBACK_APPLIED && call.state !== CallState.TERMINAL) this.callbacks.set(wireId, call);
    return call;
  }

  transition(to) {
    if (!allowedRoundTransition(this.state, to)) {
      throw new ToolRoundError("invalid_round_transition", `invalid ToolRound transition ${this.state} -> ${to}`, 409);
    }
    if (this.state === to) return;
    const next = this.toRecord();
    next.state = to;
    this.persistRecord(next);
    this.state = to;
  }

  markAwaitingResults() {
    if (this.state === RoundState.COLLECTING) this.transition(RoundState.AWAITING_RESULTS);
  }

  markHanded(wireId) {
    const call = this.calls.get(wireId);
    if (!call) throw new ToolRoundError("unknown_tool_call_id", `round ${this.route} does not contain ${wireId}`, 410);
    if (call.state === CallState.HANDED_TO_TRANSPORT || call.state === CallState.RESULT_RECEIVED || call.state === CallState.CALLBACK_APPLIED) return call;
    if (call.state !== CallState.REGISTERED) throw new ToolRoundError("call_not_registered", `tool ${wireId} cannot be handed while ${call.state}`, 409);
    const handedAt = this.clock();
    const next = this.toRecord();
    const saved = next.calls.find((item) => item.wireId === wireId);
    saved.state = CallState.HANDED_TO_TRANSPORT;
    saved.handedAt = handedAt;
    this.persistRecord(next);
    call.state = CallState.HANDED_TO_TRANSPORT;
    call.handedAt = handedAt;
    this.outbound = this.outbound.filter((id) => id !== wireId);
    if (!this.batch.includes(wireId)) this.batch.push(wireId);
    return call;
  }

  startTimer(wireId, timeoutMs, onTimeout) {
    const call = this.calls.get(wireId);
    if (!call || call.timer || call.state !== CallState.HANDED_TO_TRANSPORT) return false;
    call.timer = this.timers.setTimeout(() => {
      call.timer = null;
      onTimeout(call);
    }, timeoutMs);
    return true;
  }

  clearTimer(call) {
    if (!call || !call.timer) return;
    this.timers.clearTimeout(call.timer);
    call.timer = null;
  }

  prepareDeferredInput(clientMessageId, input) {
    let id = typeof clientMessageId === "string" ? clientMessageId.trim() : "";
    const intent = canonicalDeferredInput(input);
    const recoveryContext = canonicalDeferredContext(input);
    validateDeferredSizes(intent, recoveryContext);
    if (!intent.text && intent.images.length === 0) {
      return { existing: null, saved: null, receipt: { clientMessageId: null, status: "none" } };
    }
    if (!id || id.length > 256 || /[\u0000-\u001f\u007f]/.test(id)) {
      throw new ToolRoundError("invalid_client_message_id", "mixed tool/user continuation requires a stable clientMessageId", 422);
    }
    const hash = hashDeferredInput(intent);
    const requestedClientMessageId = id;
    let existing = this.deferredInputs.find((item) => item.clientMessageId === id);
    let rekeyed = false;
    if (existing) {
      if (existing.hash !== hash) {
        const routed = rekeyConflictingClientMessageId(id, hash, this.deferredInputs);
        id = routed.candidate;
        existing = routed.existing || null;
        rekeyed = true;
      }
      if (existing) {
        return {
          existing,
          saved: null,
          recoveryContext,
          receipt: {
            clientMessageId: id,
            duplicate: true,
            status: rekeyed ? "rekeyed_duplicate" : existing.state.toLowerCase(),
            ...(rekeyed ? { requestedClientMessageId, rekeyed: true } : {}),
          },
        };
      }
    }
    const saved = {
      clientMessageId: id,
      deliveredAt: null,
      deliveryStartedAt: null,
      hash,
      input: intent,
      ...(rekeyed ? { requestedClientMessageId } : {}),
      receivedAt: this.clock(),
      state: DeferredInputState.QUEUED,
    };
    return {
      existing: null,
      saved,
      recoveryContext,
      receipt: {
        clientMessageId: id,
        duplicate: false,
        status: rekeyed ? "rekeyed_queued" : "queued",
        ...(rekeyed ? { requestedClientMessageId, rekeyed: true } : {}),
      },
    };
  }

  stagePreparedDeferred(next, prepared) {
    if (!prepared) return false;
    // A delayed replay of an already-delivered/superseded id must not roll the
    // round's shared recovery context back to an older stateless snapshot.
    // Context may advance only with a new intent or with the currently
    // actionable retry of that same intent.
    const existingIsActionable = prepared.existing && (
      prepared.existing.state === DeferredInputState.QUEUED
      || prepared.existing.state === DeferredInputState.DELIVERING
    );
    const mayAdvanceContext = !!prepared.saved || !!existingIsActionable;
    const targetContext = mayAdvanceContext
      ? (prepared.recoveryContext || canonicalDeferredContext(null))
      : (next.recoveryContext || canonicalDeferredContext(null));
    let changed = mayAdvanceContext && canonicalJSONString(next.recoveryContext || canonicalDeferredContext(null))
      !== canonicalJSONString(targetContext);
    if (prepared.saved) {
      for (const entry of next.deferredInputs) {
        if (entry.state !== DeferredInputState.QUEUED && entry.state !== DeferredInputState.DELIVERING) continue;
        const uncertain = entry.state === DeferredInputState.DELIVERING;
        entry.state = DeferredInputState.SUPERSEDED;
        entry.input = null;
        entry.supersededAt = prepared.saved.receivedAt;
        entry.supersededBy = prepared.saved.clientMessageId;
        if (uncertain) entry.supersedeReason = "delivery_uncertain_superseded";
      }
      next.deferredInputs.push(prepared.saved);
      changed = true;
    }
    if (!changed) return false;
    next.recoveryContext = targetContext;
    const compacted = compactDeferredState(next);
    next.deferredInputs = compacted.deferredInputs;
    next.recoveryContext = compacted.recoveryContext;
    return true;
  }

  adoptDeferredRecord(record) {
    this.deferredInputs = record.deferredInputs.map((entry) => ({ ...entry }));
    this.recoveryContext = record.recoveryContext;
  }

  queueDeferredInput(clientMessageId, input) {
    const prepared = this.prepareDeferredInput(clientMessageId, input);
    const next = this.toRecord();
    if (!this.stagePreparedDeferred(next, prepared)) return prepared.receipt;
    this.persistRecord(next);
    this.adoptDeferredRecord(next);
    return prepared.receipt;
  }

  deferredInput(clientMessageId) {
    return this.deferredInputs.find((item) => item.clientMessageId === clientMessageId) || null;
  }

  markDeferredInputState(clientMessageId, state, metadata = null) {
    const item = this.deferredInput(clientMessageId);
    if (!item) throw new ToolRoundError("unknown_client_message_id", `round ${this.route} does not contain client message ${clientMessageId}`, 410);
    const meta = metadata && typeof metadata === "object" ? metadata : {};
    if (item.state === state && Object.keys(meta).length === 0) return item;
    const allowed = item.state === DeferredInputState.QUEUED && state === DeferredInputState.DELIVERING
      || item.state === DeferredInputState.DELIVERING && state === DeferredInputState.DELIVERED
      || (item.state === DeferredInputState.QUEUED || item.state === DeferredInputState.DELIVERING)
        && state === DeferredInputState.SUPERSEDED
      || item.state === DeferredInputState.SUPERSEDED
        && item.supersedeReason === "delivery_uncertain_superseded"
        && state === DeferredInputState.DELIVERED
      // Delivery evidence can race: the SDK may emit user-message-appended
      // immediately before agent.send resolves. Enriching the same state is
      // idempotent and must not turn two independent success signals into a
      // client-visible state conflict.
      || item.state === state && (state === DeferredInputState.DELIVERING
        || state === DeferredInputState.DELIVERED);
    if (!allowed) {
      throw new ToolRoundError("client_message_state_conflict", `client message ${clientMessageId} cannot transition from ${item.state} to ${state}`, 409);
    }
    const at = this.clock();
    const next = this.toRecord();
    const saved = next.deferredInputs.find((entry) => entry.clientMessageId === clientMessageId);
    saved.state = state;
    if (state === DeferredInputState.DELIVERING) {
      saved.deliveryStartedAt ||= at;
      saved.deliveryAttempts = (saved.deliveryAttempts || 0) + (item.state === DeferredInputState.DELIVERING ? 0 : 1);
      if (typeof meta.agentId === "string" && meta.agentId) saved.deliveryAgentId = meta.agentId;
      if (typeof meta.textHash === "string" && /^[a-f0-9]{64}$/.test(meta.textHash)) saved.deliveryTextHash = meta.textHash;
      if (typeof meta.hasImages === "boolean") saved.deliveryHasImages = meta.hasImages;
    }
    if (state === DeferredInputState.DELIVERED) {
      saved.deliveredAt ||= at;
      if (typeof meta.evidence === "string" && meta.evidence) saved.deliveryEvidence ||= meta.evidence;
    }
    if (state === DeferredInputState.SUPERSEDED) {
      saved.supersededAt = at;
      if (typeof meta.reason === "string" && meta.reason) saved.supersedeReason = meta.reason;
      saved.input = null;
    }
    this.persistRecord(next);
    Object.assign(item, saved);
    return item;
  }

  toolEpochState() {
    const calls = [...this.calls.values()];
    return {
      route: this.route,
      runEpoch: this.runEpoch,
      roundSeq: this.roundSeq,
      roundState: this.state,
      pending: calls.filter((call) => !call.receipt).map((call) => call.wireId),
      receipted: calls.filter((call) => !!call.receipt).map((call) => call.wireId),
      applied: calls.filter((call) => !!call.callbackAppliedAt).map((call) => call.wireId),
      terminalReason: this.terminal && this.terminal.reason || null,
    };
  }

  preflightResults(batch, {
    allowRegisteredReceipt = false,
    preferDurableReceipt = false,
  } = {}) {
    if (!Array.isArray(batch) || batch.length === 0) {
      throw new ToolRoundError("empty_tool_results", "continuation contains no tool results", 422);
    }
    const byId = new Map();
    for (const raw of batch) {
      const id = raw && raw.toolCallId;
      if (!id) throw new ToolRoundError("missing_tool_call_id", "every tool result must carry its emitted tool-call id", 422);
      if (this.codec) {
        const parsed = this.codec.parse(id);
        if (parsed.route !== this.route) throw new ToolRoundError("mixed_tool_rounds", "tool-result batch spans multiple rounds", 409);
      }
      const call = this.calls.get(id);
      if (!call) throw new ToolRoundError("unknown_tool_call_id", `round ${this.route} does not contain ${id}`, 410);
      const semanticResult = canonicalResult(raw);
      let result = deliveryResultFromSemantic(semanticResult);
      let hash = hashCanonicalResult(semanticResult);
      const storedHash = call.resultHash
        ? call.resultHashVersion === TOOL_RESULT_CANONICAL_VERSION
          ? call.resultHash
          : call.semanticResultHash || (call.receipt && call.receipt.result
            ? hashClientToolResult(legacyReceiptSemanticInput(call.receipt.result))
            : call.resultHash)
        : null;
      let durableReceiptRetained = false;
      if (storedHash && storedHash !== hash) {
        // Receipt persistence is the exactly-once boundary. Stateless
        // OpenAI/Anthropic clients may replay the same historical tool message
        // with a lossy or enriched envelope while parallel calls are still
        // live. First-write wins: retain the durable value and never replace
        // or deliver the later projection to an SDK callback.
        if (preferDurableReceipt && call.receipt && call.receipt.result) {
          hash = storedHash;
          result = call.receipt.result;
          durableReceiptRetained = true;
        } else {
          throw new ToolRoundError("result_conflict", `tool result ${id} conflicts with its durable receipt`, 409);
        }
      }
      const priorInBatch = byId.get(id);
      if (priorInBatch && priorInBatch.hash !== hash) {
        if (preferDurableReceipt) {
          // One HTTP projection can accidentally repeat a tool result with
          // two incompatible envelopes. There is no safe way to apply both;
          // preserve deterministic first-occurrence ordering, record the
          // discrepancy in the receipt, and never turn it into a retry poison.
          priorInBatch.sameBatchConflictIgnored = true;
          continue;
        }
        throw new ToolRoundError("result_conflict", `tool result ${id} occurs twice with different payloads`, 409);
      }
      if (!call.resultHash && !call.handedAt && !allowRegisteredReceipt) {
        throw new ToolRoundError("result_before_handoff", `tool result ${id} arrived before its call was handed to the client`, 409);
      }
      byId.set(id, { call, hash, result, storedHash, durableReceiptRetained });
    }
    return [...byId.values()];
  }

  commitResults(batch, {
    allowTerminalReceipt = false,
    allowRegisteredReceipt = false,
    preferDurableReceipt = false,
    deferredInputId = "",
    deferredInput = null,
  } = {}) {
    const prepared = this.preflightResults(batch, {
      allowRegisteredReceipt,
      preferDurableReceipt,
    });
    // Validate the optional user intent before constructing or persisting any
    // result receipt. A rejected continuation is therefore a true no-op: it
    // cannot grow the journal while returning the same 4xx forever.
    const preparedDeferred = deferredInput
      ? this.prepareDeferredInput(deferredInputId, deferredInput)
      : null;
    const additions = prepared.filter(({ call }) => !call.resultHash);
    const migrations = prepared.filter(({ call, hash, durableReceiptRetained }) => !durableReceiptRetained && call.resultHash
      && call.resultHashVersion !== TOOL_RESULT_CANONICAL_VERSION
      && call.semanticResultHash !== hash);
    const dispositionFor = ({ call, durableReceiptRetained, sameBatchConflictIgnored }) => ({
      toolCallId: call.wireId,
      status: sameBatchConflictIgnored
        ? "first_batch_result_retained"
        : durableReceiptRetained
        ? "durable_receipt_retained"
        : additions.some((item) => item.call === call) ? "committed" : "duplicate",
      callbackState: call.callbackAppliedAt ? "applied" : call.callback ? "pending" : "absent",
      ...(sameBatchConflictIgnored ? { ignoredConflictingDuplicate: true } : {}),
    });
    if (additions.length > 0 && this.state !== RoundState.AWAITING_RESULTS && this.state !== RoundState.APPLYING_RESULTS
      && !(allowRegisteredReceipt && this.state === RoundState.COLLECTING)
      && !(allowTerminalReceipt && this.state === RoundState.TERMINAL)) {
      throw new ToolRoundError("round_not_awaiting_results", `round ${this.route} cannot receive new results while ${this.state}`, 409);
    }
    const next = this.toRecord();
    const deferredChanged = this.stagePreparedDeferred(next, preparedDeferred);
    if (additions.length > 0 && this.state !== RoundState.TERMINAL) next.state = RoundState.APPLYING_RESULTS;
    const durableReceipts = new Map();
    for (const { call, hash, result } of additions) {
      const saved = next.calls.find((item) => item.wireId === call.wireId);
      saved.receipt = {
        acceptedAt: this.clock(),
        canonicalVersion: TOOL_RESULT_CANONICAL_VERSION,
        result,
      };
      durableReceipts.set(call.wireId, saved.receipt);
      saved.resultHash = hash;
      saved.resultHashVersion = TOOL_RESULT_CANONICAL_VERSION;
      saved.semanticResultHash = hash;
      saved.state = this.state === RoundState.TERMINAL ? CallState.TERMINAL : CallState.RESULT_RECEIVED;
    }
    for (const { call, hash } of migrations) {
      const saved = next.calls.find((item) => item.wireId === call.wireId);
      saved.resultHashVersion = saved.resultHashVersion || 1;
      saved.semanticResultHash = hash;
    }
    const needsPersist = additions.length > 0 || migrations.length > 0 || deferredChanged;
    if (needsPersist) this.persistRecord(next);
    if (additions.length > 0 && this.state !== RoundState.TERMINAL) this.state = RoundState.APPLYING_RESULTS;
    for (const { call, hash } of additions) {
      call.receipt = durableReceipts.get(call.wireId);
      call.resultHash = hash;
      call.resultHashVersion = TOOL_RESULT_CANONICAL_VERSION;
      call.semanticResultHash = hash;
      if (this.state !== RoundState.TERMINAL) call.state = CallState.RESULT_RECEIVED;
      this.clearTimer(call);
    }
    for (const { call, hash } of migrations) {
      call.resultHashVersion = call.resultHashVersion || 1;
      call.semanticResultHash = hash;
    }
    if (deferredChanged) this.adoptDeferredRecord(next);
    return {
      additions: additions.map(({ call }) => call.wireId),
      duplicates: prepared.filter(({ call }) => !!call.resultHash && !additions.some((item) => item.call === call)).map(({ call }) => call.wireId),
      retainedConflicts: prepared.filter(({ durableReceiptRetained }) => durableReceiptRetained).map(({ call }) => call.wireId),
      ignoredSameBatchConflicts: prepared.filter(({ sameBatchConflictIgnored }) => sameBatchConflictIgnored).map(({ call }) => call.wireId),
      dispositions: prepared.map(dispositionFor),
      deferredReceipt: preparedDeferred ? preparedDeferred.receipt : null,
    };
  }

  applyCommittedCallbacks(ids = null) {
    const wanted = ids ? new Set(ids) : null;
    const applied = [];
    for (const id of this.fifo) {
      if (wanted && !wanted.has(id)) continue;
      const call = this.calls.get(id);
      if (!call || !call.receipt || call.callbackAppliedAt || (!call.callback && !call.waiters.length)) continue;
      this.clearTimer(call);
      try {
        for (const callback of [call.callback, ...call.waiters].filter(Boolean)) {
          callback.resolve(call.receipt.result);
        }
      } catch (error) {
        for (const callback of [call.callback, ...call.waiters].filter(Boolean)) {
          try { callback.reject(error); } catch {}
        }
        call.callback = null;
        call.waiters = [];
        this.callbacks.delete(id);
        this.terminalize(TerminalReason.RUN_ERROR, `callback application failed for ${id}: ${error.message}`);
        throw new ToolRoundError("callback_apply_failed", `failed to apply tool result ${id}: ${error.message}`, 500);
      }
      call.callbackAppliedAt = this.clock();
      call.state = CallState.CALLBACK_APPLIED;
      call.callback = null;
      call.waiters = [];
      this.callbacks.delete(id);
      applied.push(id);
    }
    if (applied.length) {
      const next = this.toRecord();
      next.state = this.callbacks.size === 0 ? RoundState.RESUMING : RoundState.AWAITING_RESULTS;
      this.persistRecord(next);
      this.state = next.state;
    } else if (this.state === RoundState.APPLYING_RESULTS) {
      this.transition(this.callbacks.size === 0 ? RoundState.RESUMING : RoundState.AWAITING_RESULTS);
    }
    return applied;
  }

  applyResults(batch) {
    const committed = this.commitResults(batch);
    const applied = this.applyCommittedResults(committed.additions.length ? committed.additions : committed.duplicates);
    return { ok: true, ...committed, applied };
  }

  applyCommittedResults(ids = null) {
    const applied = this.applyCommittedCallbacks(ids);
    if (this.callbacks.size === 0 && this.calls.size > 0 && [...this.calls.values()].every((call) => call.receipt)) {
      this.terminalize(TerminalReason.COMPLETED);
    }
    return applied;
  }

  terminalize(reason, detail = "") {
    if (this.state === RoundState.TERMINAL) return false;
    const at = this.clock();
    const next = this.toRecord();
    next.state = RoundState.TERMINAL;
    next.terminal = { at, detail: String(detail || ""), reason };
    for (const saved of next.calls) {
      if (saved.state !== CallState.CALLBACK_APPLIED) saved.state = CallState.TERMINAL;
      saved.terminalAt = at;
    }
    this.persistRecord(next);
    this.state = RoundState.TERMINAL;
    this.terminal = next.terminal;
    for (const call of this.calls.values()) {
      this.clearTimer(call);
      if (call.state !== CallState.CALLBACK_APPLIED && call.callback) {
        try { call.callback.reject(new Error(`[bridge] ${reason}${detail ? `: ${detail}` : ""}`)); } catch {}
      }
      if (call.state !== CallState.CALLBACK_APPLIED) {
        for (const callback of call.waiters) {
          try { callback.reject(new Error(`[bridge] ${reason}${detail ? `: ${detail}` : ""}`)); } catch {}
        }
      }
      call.callback = null;
      call.waiters = [];
      if (call.state !== CallState.CALLBACK_APPLIED) call.state = CallState.TERMINAL;
      call.terminalAt = at;
      this.callbacks.delete(call.wireId);
    }
    if (this.terminalCallback) this.terminalCallback(this);
    return true;
  }

  recordRecovery(recovery) {
    const next = this.toRecord();
    next.recovery = jsonValue(recovery);
    this.persistRecord(next);
    this.recovery = next.recovery;
  }
}

// At process startup no in-memory SDK callback can still be attached. Convert
// every nonterminal snapshot into a restart-lost tombstone so retention can
// eventually bound it. A late authenticated result may still add a durable
// receipt and take the faithful recovery or refusal path.
export function terminalizeOrphanedRounds(journal, codec, detail = "bridge restarted before the paused callback completed") {
  let terminalized = 0;
  for (const record of journal.records()) {
    if (!record || record.state === RoundState.TERMINAL) continue;
    const round = new ToolRound({ journal, codec, record });
    if (round.terminalize(TerminalReason.RESTART_LOST, detail)) terminalized++;
  }
  return terminalized;
}

export function createTestRound(overrides = {}) {
  const secret = overrides.secret || Buffer.alloc(32, 7);
  return new ToolRound({
    sessionId: "test-session",
    agentId: "test-agent",
    runEpoch: 1,
    roundSeq: 1,
    codec: new RoutingTokenCodec(secret),
    ...overrides,
  });
}
