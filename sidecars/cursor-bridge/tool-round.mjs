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
    const out = {};
    for (const key of Object.keys(value).sort()) {
      if (value[key] === undefined) throw new ToolRoundError("non_json_result", `undefined value at ${at}.${key}`, 422);
      out[key] = jsonValue(value[key], `${at}.${key}`);
    }
    return out;
  }
  throw new ToolRoundError("non_json_result", `unsupported ${typeof value} at ${at}`, 422);
}

export function canonicalJSONString(value) {
  return JSON.stringify(jsonValue(value));
}

export function canonicalResult(result) {
  return {
    content: result && Object.prototype.hasOwnProperty.call(result, "content") ? result.content : "",
    images: result && Array.isArray(result.images) ? result.images : [],
    isError: !!(result && result.isError === true),
    structuredContent: result && Object.prototype.hasOwnProperty.call(result, "structuredContent")
      ? result.structuredContent
      : null,
  };
}

export function hashClientToolResult(result) {
  return createHash("sha256").update(canonicalJSONString(canonicalResult(result))).digest("hex");
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
      if (record?.state === RoundState.TERMINAL) terminal.push(record);
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
        if (current?.state !== RoundState.TERMINAL) continue;
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
    name: call.name,
    ordinal: call.ordinal,
    rawIdHash: call.rawIdHash,
    receipt: call.receipt,
    resultHash: call.resultHash,
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
    timer: null,
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
      this.calls = new Map(record.calls.map((saved) => [saved.wireId, callFromRecord(saved)]));
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

  openCall({ source = "sdk", rawToolCallId = null, name, input, callback = null }) {
    // The SDK may dispatch the tail of one parallel wave after the bridge has
    // already handed the first batch and entered AWAITING_RESULTS. Keep that
    // tail in this same durable round; Session will journal it for the next
    // response instead of writing a tool_call after turn_end.
    if (this.state !== RoundState.COLLECTING && this.state !== RoundState.AWAITING_RESULTS) {
      throw new ToolRoundError("round_not_collecting", `round ${this.route} cannot open calls while ${this.state}`, 409);
    }
    if (!name) throw new ToolRoundError("tool_name_required", "client tool name is required", 500);
    const rawIdHash = rawToolCallId == null ? null : createHash("sha256").update(String(rawToolCallId)).digest("hex");
    if (rawIdHash && this.rawToCall.has(rawIdHash)) {
      const existing = this.calls.get(this.rawToCall.get(rawIdHash));
      if (existing.name !== name || canonicalJSONString(existing.input) !== canonicalJSONString(input)) {
        throw new ToolRoundError("raw_tool_id_conflict", "SDK reused a tool-call id with incompatible name or input", 409);
      }
      if (callback && !existing.callback) {
        existing.callback = callback;
        this.callbacks.set(existing.wireId, existing);
      }
      return existing;
    }
    const ordinal = this.fifo.length;
    const call = {
      callback,
      callbackAppliedAt: null,
      handedAt: null,
      input: jsonValue(input == null ? input : JSON.parse(JSON.stringify(input))),
      name: String(name),
      ordinal,
      rawIdHash,
      receipt: null,
      resultHash: null,
      source: String(source || "sdk"),
      state: CallState.REGISTERED,
      terminalAt: null,
      timer: null,
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
    if (call.callback && call.callback !== callback) throw new ToolRoundError("callback_conflict", `tool ${wireId} already has a live callback`, 409);
    call.callback = callback;
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

  preflightResults(batch, { allowRegisteredReceipt = false } = {}) {
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
      const result = canonicalResult(raw);
      const hash = hashClientToolResult(result);
      const priorInBatch = byId.get(id);
      if (priorInBatch && priorInBatch.hash !== hash) {
        throw new ToolRoundError("result_conflict", `tool result ${id} occurs twice with different payloads`, 409);
      }
      if (call.resultHash && call.resultHash !== hash) {
        throw new ToolRoundError("result_conflict", `tool result ${id} conflicts with its durable receipt`, 409);
      }
      if (!call.resultHash && !call.handedAt && !allowRegisteredReceipt) {
        throw new ToolRoundError("result_before_handoff", `tool result ${id} arrived before its call was handed to the client`, 409);
      }
      byId.set(id, { call, hash, result });
    }
    return [...byId.values()];
  }

  commitResults(batch, { allowTerminalReceipt = false, allowRegisteredReceipt = false } = {}) {
    const prepared = this.preflightResults(batch, { allowRegisteredReceipt });
    const additions = prepared.filter(({ call }) => !call.resultHash);
    if (additions.length === 0) return { additions: [], duplicates: prepared.map(({ call }) => call.wireId) };
    if (this.state !== RoundState.AWAITING_RESULTS && this.state !== RoundState.APPLYING_RESULTS
      && !(allowRegisteredReceipt && this.state === RoundState.COLLECTING)
      && !(allowTerminalReceipt && this.state === RoundState.TERMINAL)) {
      throw new ToolRoundError("round_not_awaiting_results", `round ${this.route} cannot receive new results while ${this.state}`, 409);
    }
    const next = this.toRecord();
    if (this.state !== RoundState.TERMINAL) next.state = RoundState.APPLYING_RESULTS;
    const durableReceipts = new Map();
    for (const { call, hash, result } of additions) {
      const saved = next.calls.find((item) => item.wireId === call.wireId);
      saved.receipt = { acceptedAt: this.clock(), result };
      durableReceipts.set(call.wireId, saved.receipt);
      saved.resultHash = hash;
      saved.state = this.state === RoundState.TERMINAL ? CallState.TERMINAL : CallState.RESULT_RECEIVED;
    }
    this.persistRecord(next);
    if (this.state !== RoundState.TERMINAL) this.state = RoundState.APPLYING_RESULTS;
    for (const { call, hash } of additions) {
      call.receipt = durableReceipts.get(call.wireId);
      call.resultHash = hash;
      if (this.state !== RoundState.TERMINAL) call.state = CallState.RESULT_RECEIVED;
      this.clearTimer(call);
    }
    return {
      additions: additions.map(({ call }) => call.wireId),
      duplicates: prepared.filter(({ call }) => !!call.resultHash && !additions.some((item) => item.call === call)).map(({ call }) => call.wireId),
    };
  }

  applyCommittedCallbacks(ids = null) {
    const wanted = ids ? new Set(ids) : null;
    const applied = [];
    for (const id of this.fifo) {
      if (wanted && !wanted.has(id)) continue;
      const call = this.calls.get(id);
      if (!call || !call.receipt || call.callbackAppliedAt || !call.callback) continue;
      this.clearTimer(call);
      try {
        call.callback.resolve(call.receipt.result);
      } catch (error) {
        try { call.callback.reject(error); } catch {}
        call.callback = null;
        this.callbacks.delete(id);
        this.terminalize(TerminalReason.RUN_ERROR, `callback application failed for ${id}: ${error.message}`);
        throw new ToolRoundError("callback_apply_failed", `failed to apply tool result ${id}: ${error.message}`, 500);
      }
      call.callbackAppliedAt = this.clock();
      call.state = CallState.CALLBACK_APPLIED;
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
