import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import test from "node:test";

const TEST_STATE_ROOT = mkdtempSync(path.join(tmpdir(), "cursor-bridge-state-"));
process.env.CURSOR_AGENT_STATE_ROOT = TEST_STATE_ROOT;
process.env.CURSOR_AGENT_TOOL_BATCH_MS = "60000";
process.env.CURSOR_AGENT_PENDING_TIMEOUT_MS = "60000";

const bridge = await import("./cursor-agent-bridge.mjs");
const {
  AdvertisedToolRegistry,
  CC_CASES,
  CLIENT_TOOL_PROVIDER_ID,
  COMPOSER_MAX_INVALID_TOOL_CALLS,
  DEFAULT_MCP_SERVER_KEY,
  Session,
  authorizeRequestWith,
  bindHostIsLoopback,
  blockedNativeResult,
  blockedSyntheticNativeExecIfNeeded,
  buildMcpServers,
  buildReadSuccess,
  buildRestartRecoveryInput,
  buildWriteSuccess,
  collectToolResultImages,
  composerModelSelection,
  composerWorkspaceCwd,
  continuationTenantMismatch,
  constraintInstructions,
  dispatchMcpBatch,
  effectiveAdvertise,
  envInt,
  handleContinue,
  handleTurn,
  headlessMcpState,
  headlessRequestContext,
  healthBody,
  isConversationTooLong,
  isLoopbackRemote,
  classifyMcpRoute,
  keyFingerprint,
  liveToolRounds,
  mcpDispatch,
  mcpDispatchResult,
  mcpServerKeyForTool,
  mcpToolsForServer,
  nativeToolBlockedByChoice,
  parseShellContent,
  platforms,
  prepareAdvertisedToolRegistry,
  readinessBody,
  reconcileExport,
  refreshSessionFromBody,
  sessions,
  sdkAdvertisedTools,
  syntheticAgentArtifactFailure,
  syntheticAgentArtifactRequest,
  toolManifest,
  toolManifestRule,
  toSdkImages,
  truncateLiveToolResult,
  typedUnavailableResult,
  validateBindHost,
  wrapToolInput,
} = bridge;

class MockResponse extends EventEmitter {
  constructor(writeReturns = []) {
    super();
    this.writeReturns = [...writeReturns];
    this.status = 0;
    this.headers = {};
    this.headersSent = false;
    this.chunks = [];
    this.ended = false;
  }
  setHeader(name, value) { this.headers[String(name).toLowerCase()] = value; }
  writeHead(status, headers = {}) {
    this.status = status;
    this.headersSent = true;
    for (const [key, value] of Object.entries(headers)) this.setHeader(key, value);
    return this;
  }
  write(chunk) {
    this.chunks.push(String(chunk));
    return this.writeReturns.length ? this.writeReturns.shift() : true;
  }
  end(chunk = undefined) {
    if (chunk !== undefined) this.chunks.push(String(chunk));
    this.ended = true;
  }
  text() { return this.chunks.join(""); }
  json() { return JSON.parse(this.text()); }
}

function request(remoteAddress = "127.0.0.1") {
  const req = new EventEmitter();
  req.headers = {};
  req.socket = { remoteAddress };
  return req;
}

function neutralBody(body = {}) {
  return { ...body, clientEnv: { workspaceUnknown: true } };
}

function advertised(name = "Lookup") {
  return [{ name, toolName: name, description: `${name} tool`, inputSchema: { type: "object", properties: { q: { type: "string" } } } }];
}

function seedSession(id, key = "cursor-key", tools = advertised()) {
  const session = new Session(id, key);
  session.clientEnv = { workspaceUnknown: true };
  session.advertise = tools;
  const output = new MockResponse();
  session.beginResponse(output);
  sessions.set(id, session);
  return { session, output };
}

async function waitFor(predicate, message = "condition") {
  for (let i = 0; i < 100; i++) {
    if (predicate()) return;
    await new Promise((resolve) => setImmediate(resolve));
  }
  assert.fail(`timed out waiting for ${message}`);
}

async function openTool(session, { name = "Lookup", rawId = "sdk-call", input = { q: "x" }, adapter = null, awaiting = true } = {}) {
  const promise = session.openClientTool({ source: "test", rawToolCallId: rawId, name, input, resultAdapter: adapter || ((value) => value) });
  promise.catch(() => {});
  await waitFor(() => session.currentRound && session.currentRound.fifo.length === 1, "journaled tool call");
  const round = session.currentRound;
  const call = round.calls.get(round.fifo[0]);
  await waitFor(() => call.handedAt != null, "transport handoff receipt");
  if (session.flushTimer) { clearTimeout(session.flushTimer); session.flushTimer = null; }
  if (awaiting) round.markAwaitingResults();
  return { call, promise, round };
}

function continuationBody(results, extraInput = {}, extraBody = {}) {
  return neutralBody({
    model: "cursor-grok-4.5",
    input: { type: "tool_results", results, ...extraInput },
    ...extraBody,
  });
}

function journalRecord(route) {
  return JSON.parse(readFileSync(path.join(TEST_STATE_ROOT, "client-tool-rounds", `${route}.json`), "utf8"));
}

async function cleanupState() {
  for (const session of sessions.values()) {
    try { await session.cancel(); } catch {}
  }
  sessions.clear();
  liveToolRounds.clear();
  platforms.clear();
}

test.beforeEach(cleanupState);
test.after(async () => {
  await cleanupState();
  rmSync(TEST_STATE_ROOT, { recursive: true, force: true });
});

test("workspace identity is advisory and unknown context never invents a server path", () => {
  const context = headlessRequestContext({ clientEnv: { workspaceUnknown: true }, advertiseForGating: () => [] })
    .__ccJson.success.requestContext;
  const env = context.env;
  assert.deepEqual(env.workspacePaths, []);
  assert.equal(env.processWorkingDirectory, undefined);
  assert.equal(env.projectFolder, undefined);
  assert.equal(composerWorkspaceCwd(null), "");
  assert.equal(composerWorkspaceCwd({ workspaceUnknown: true }), "");
  assert.equal(toolManifestRule(advertised(), ""), null);
  assert.equal(context.mcpMetaToolOptions.enabled, false);
  assert.doesNotMatch(JSON.stringify(env), /\/workspace|\/app/);
});

test("known workspace context is projected without changing its path", () => {
  const clientEnv = { workspacePaths: ["/home/me/repo"], processWorkingDirectory: "/home/me/repo", shell: "zsh" };
  const env = headlessRequestContext({ clientEnv, advertiseForGating: () => [] }).__ccJson.success.requestContext.env;
  assert.deepEqual(env.workspacePaths, ["/home/me/repo"]);
  assert.equal(env.processWorkingDirectory, "/home/me/repo");
  assert.equal(env.shell, "zsh");
  assert.equal(composerWorkspaceCwd(clientEnv), "/home/me/repo");
});

test("/agent/turn accepts missing workspace as neutral but refuses continuation on the wrong endpoint", async () => {
  const res = new MockResponse();
  await handleTurn(request(), res, { sessionId: "headerless", input: { type: "tool_results", results: [{ toolCallId: "x", content: "y" }] } }, "key");
  assert.equal(res.status, 400);
  assert.match(res.text(), /\/agent\/continue/);
  assert.notEqual(res.status, 422);
});

test("/agent/turn rejects null and array JSON bodies without throwing", async () => {
  for (const body of [null, []]) {
    const res = new MockResponse();
    await handleTurn(request(), res, body, "key");
    assert.equal(res.status, 400);
    assert.match(res.text(), /JSON object/);
  }
});

test("malformed supplied workspace degrades to neutral context instead of rejecting the turn", async () => {
  const res = new MockResponse();
  await handleTurn(request(), res, { sessionId: "bad-workspace", clientEnv: { workspacePaths: [] }, input: { type: "tool_results", results: [{ toolCallId: "x" }] } }, "key");
  assert.equal(res.status, 400);
  assert.notEqual(res.status, 422);
  assert.deepEqual(sessions.get("bad-workspace"), undefined);
});

test("invalid advertised schemas fail before creating a turn session", async () => {
  const res = new MockResponse();
  await handleTurn(request(), res, neutralBody({
    sessionId: "invalid-tool-schema",
    input: { type: "user", text: "hello" },
    tools: [{ name: "Broken", inputSchema: { type: "not-a-json-schema-type" } }],
  }), "key");
  assert.equal(res.status, 422);
  assert.match(res.text(), /invalid inputSchema/);
  assert.equal(sessions.has("invalid-tool-schema"), false);
});

test("unknown workspace never leaks the SDK isolation directory through shell result metadata", () => {
  const unary = CC_CASES.shellArgs.buildResult("ok", { command: "pwd" }, false, { cwd: "" });
  const stream = CC_CASES.shellStreamArgs.buildChunks("ok", false, { cwd: "" });
  assert.equal(unary.success.workingDirectory, "");
  assert.equal(stream.at(-1).exit.cwd, "");
  assert.doesNotMatch(JSON.stringify({ unary, stream }), /\.cursor-agent-store|\/app|\/workspace/);
});

test("advertised tools have one normalized registry and duplicate names fail closed", () => {
  const registry = new AdvertisedToolRegistry();
  registry.replace([{ name: "A" }, { toolName: "B", inputSchema: { type: "object" } }]);
  assert.deepEqual(registry.all().map((tool) => tool.name), ["A", "B"]);
  assert.deepEqual(registry.find("A").inputSchema, { type: "object" });
  assert.throws(() => registry.replace([{ name: "A" }, { toolName: "A" }]), /appears more than once/);
  assert.throws(() => registry.replace([{}]), /missing a string name/);
  assert.throws(() => registry.replace([{ name: "Broken", inputSchema: { type: "definitely-not-json-schema" } }]), /invalid inputSchema/);
  assert.throws(() => registry.replace([{ name: "Async", inputSchema: { $async: true, type: "object" } }]), /asynchronous JSON Schema/);
});

test("private descriptor aliases survive body refresh, route explicitly, and never reach the SDK inventory", () => {
  const registry = prepareAdvertisedToolRegistry({ tools: [{
    name: "RunCommand",
    description: "Run a command",
    aliases: ["shell", "terminal", "shell"],
    inputSchema: {
      type: "object",
      properties: { command: { type: "string" } },
      required: ["command"],
      additionalProperties: false,
    },
  }] });
  const session = new Session("private-aliases", "key");
  session.toolRegistry = registry;
  assert.deepEqual(registry.find("RunCommand").aliases, ["shell", "terminal"]);
  assert.equal(session.reconcileToolName("shell"), "RunCommand");
  assert.equal(session.reconcileToolName("terminal"), "RunCommand");
  assert.equal(Object.hasOwn(sdkAdvertisedTools(session)[0], "aliases"), false);
  assert.throws(() => prepareAdvertisedToolRegistry({
    tools: [{ name: "Bad", aliases: "shell", inputSchema: { type: "object" } }],
  }), /invalid private aliases/);
});

test("tool inventory refresh replaces live gating, including an explicit empty clear", () => {
  const session = new Session("dynamic-tools", "key");
  refreshSessionFromBody(session, { tools: [{ name: "A", inputSchema: { type: "object" } }], toolChoice: "auto" });
  session.activeAdvertise = effectiveAdvertise(session.advertise, "auto");
  assert.deepEqual(session.advertiseForGating().map((tool) => tool.name), ["A"]);

  refreshSessionFromBody(session, { tools: [{ name: "B", inputSchema: { type: "object" } }], toolChoice: "auto" });
  assert.deepEqual(session.advertiseForGating().map((tool) => tool.name), ["B"]);

  refreshSessionFromBody(session, { tools: [], toolChoice: "none" });
  assert.deepEqual(session.advertise, []);
  assert.deepEqual(session.advertiseForGating(), []);

  refreshSessionFromBody(session, {
    tools: [{ name: "B", inputSchema: { type: "object" } }, { name: "C", inputSchema: { type: "object" } }],
    toolChoice: "specific:B",
  });
  assert.deepEqual(session.advertiseForGating().map((tool) => tool.name), ["B"]);
  refreshSessionFromBody(session, {
    tools: [{ name: "B", inputSchema: { type: "object" } }, { name: "C", inputSchema: { type: "object" } }],
  });
  assert.deepEqual(session.advertiseForGating().map((tool) => tool.name), ["B", "C"]);
});

test("boolean false tool schemas fail closed instead of widening to an executable object", () => {
  assert.throws(() => prepareAdvertisedToolRegistry({
    tools: [{ name: "Never", inputSchema: false }],
  }), /inputSchema:false/);
  const registry = new AdvertisedToolRegistry();
  registry.replace([{ name: "Never", inputSchema: false }]);
  const normalized = registry.normalize("Never", {});
  const failure = registry.validate("Never", normalized.value);
  assert.equal(failure.structuredContent.executed, false);
});

test("advertised schema validation honors declared modern JSON Schema dialects", () => {
  const registry = new AdvertisedToolRegistry();
  registry.replace([{
    name: "Modern",
    inputSchema: {
      $schema: "https://json-schema.org/draft/2020-12/schema",
      type: "object",
      properties: {
        pair: {
          type: "array",
          prefixItems: [{ type: "string" }, { type: "number" }],
          items: false,
        },
      },
      required: ["pair"],
    },
  }]);
  assert.equal(registry.validate("Modern", { pair: ["x", 1] }), null);
  assert.equal(registry.validate("Modern", { pair: ["x", "wrong"] }).structuredContent.errors[0].path, "pair.1");
});

test("patched advertisement and MCP inventory consume the same registry", () => {
  const { session } = seedSession("registry", "key", [
    { name: "mcp__memory__search", toolName: "mcp__memory__search", description: "search", inputSchema: { type: "object" } },
    { name: "Bash", toolName: "Bash", description: "shell", inputSchema: { type: "object" } },
  ]);
  const servers = buildMcpServers(session);
  const state = headlessMcpState(session).__ccJson.success.servers;
  const requestContext = headlessRequestContext(session).__ccJson.success.requestContext;
  assert.deepEqual(Object.keys(servers).sort(), state.map((server) => server.serverName).sort());
  const inventory = state.flatMap((server) => server.tools.map((tool) => tool.name)).sort();
  assert.deepEqual(inventory, session.advertise.map((tool) => tool.name).sort());
  assert.equal(mcpServerKeyForTool("mcp__memory__search"), "memory");
  assert.equal(mcpServerKeyForTool("Bash"), DEFAULT_MCP_SERVER_KEY);
  assert.equal(mcpServerKeyForTool("_mcp__codebase_memory_mcp_index_status"), DEFAULT_MCP_SERVER_KEY);
  assert.ok(state.flatMap((server) => server.tools).every((tool) => tool.providerIdentifier === CLIENT_TOOL_PROVIDER_ID));
  assert.ok(Object.hasOwn(servers, DEFAULT_MCP_SERVER_KEY));
  assert.equal(servers[DEFAULT_MCP_SERVER_KEY].headers["X-Client-Tools-Session"], session.id);
  assert.deepEqual(sdkAdvertisedTools(session).map((tool) => tool.name).sort(), inventory);
  assert.equal(requestContext.mcpMetaToolOptions.enabled, false);
});

test("the generic MCP server carries tools added after the SDK server map is fixed without duplicates", async () => {
  const session = new Session("dynamic-registry", "key");
  session.advertise = [];
  const initiallyDialed = buildMcpServers(session);
  assert.deepEqual(Object.keys(initiallyDialed), [DEFAULT_MCP_SERVER_KEY]);
  session.mcpServerKeys = Object.freeze(Object.keys(initiallyDialed));
  session.advertise = [
    { name: "mcp__memory__search", inputSchema: { type: "object" } },
    { name: "Bash", inputSchema: { type: "object" } },
  ];
  const state = headlessMcpState(session).__ccJson.success.servers;
  assert.deepEqual(state.map((server) => server.serverName), [DEFAULT_MCP_SERVER_KEY]);
  assert.deepEqual(state[0].tools.map((tool) => tool.name).sort(), ["Bash", "mcp__memory__search"]);
  sessions.set(session.id, session);
  const listed = await mcpDispatch({ jsonrpc: "2.0", id: 32, method: "tools/list" }, session.id, "");
  assert.deepEqual(listed.result.tools.map((tool) => tool.name).sort(), ["Bash", "mcp__memory__search"]);
});

test("new natural MCP servers overflow into generic while existing servers keep one exact copy", () => {
  const session = new Session("dynamic-natural-registry", "key");
  session.advertise = [{ name: "mcp__memory__search", inputSchema: { type: "object" } }];
  session.mcpServerKeys = Object.freeze(Object.keys(buildMcpServers(session)));
  assert.deepEqual([...session.mcpServerKeys].sort(), [DEFAULT_MCP_SERVER_KEY, "memory"].sort());
  session.advertise = [
    { name: "mcp__memory__search", inputSchema: { type: "object" } },
    { name: "mcp__github__get_issue", inputSchema: { type: "object" } },
    { name: "Read", inputSchema: { type: "object" } },
  ];
  const state = headlessMcpState(session).__ccJson.success.servers;
  const byServer = Object.fromEntries(state.map((server) => [server.serverName, server.tools.map((tool) => tool.name).sort()]));
  assert.deepEqual(byServer.memory, ["mcp__memory__search"]);
  assert.deepEqual(byServer[DEFAULT_MCP_SERVER_KEY], ["Read", "mcp__github__get_issue"]);
  assert.equal(state.flatMap((server) => server.tools).length, 3);
});

test("reserved JavaScript property names remain valid MCP server keys", () => {
  const session = new Session("reserved-server-keys", "key");
  session.advertise = [
    { name: "mcp__constructor__lookup", inputSchema: { type: "object" } },
    { name: "mcp__toString__read", inputSchema: { type: "object" } },
  ];
  const servers = buildMcpServers(session);
  assert.ok(Object.hasOwn(servers, "constructor"));
  assert.ok(Object.hasOwn(servers, "toString"));
  const inventory = headlessMcpState(session).__ccJson.success.servers.flatMap((server) => server.tools.map((tool) => tool.name));
  assert.deepEqual(inventory.sort(), ["mcp__constructor__lookup", "mcp__toString__read"].sort());
});

test("MCP server keys cannot become URL dot segments", () => {
  assert.equal(mcpServerKeyForTool(`mcp__${"."}__lookup`), "server-dot");
  assert.equal(mcpServerKeyForTool(`mcp__${".."}__lookup`), "server-dotdot");
});

test("tool manifest names client tools directly without teaching transport-specific wrappers", () => {
  const manifest = toolManifest([
    { name: "_mcp__codebase_memory_mcp_index_status", description: "status", inputSchema: { type: "object" } },
    { name: "Read", description: "read", inputSchema: { type: "object" } },
  ]);
  assert.match(manifest, /Available client tools this turn/);
  assert.match(manifest, /_mcp__codebase_memory_mcp_index_status/);
  assert.match(manifest, /- Read/);
  assert.doesNotMatch(manifest, /claude[- ]code/i);
  assert.doesNotMatch(manifest, /MCP server|MCP transport|transport metadata/i);
  assert.doesNotMatch(manifest, /CallMcpTool/);
  assert.doesNotMatch(manifest, /GetMcpTools/);
});

test("schema-invalid patched MCP calls are rejected before client handoff", async () => {
  const { session, output } = seedSession("invalid-patched-mcp", "key", [{
    name: "mcp__codebase_memory_mcp_index_status",
    description: "status",
    inputSchema: {
      type: "object",
      properties: { repo_path: { type: "string" }, project: { type: "string" } },
      required: ["repo_path", "project"],
      additionalProperties: false,
    },
  }]);
  const response = await session.dispatchMcp({
    toolName: "mcp__codebase_memory_mcp_index_status",
    args: {},
    toolCallId: "invalid-sdk-mcp",
  });
  assert.equal(response.__ccJson.success.isError, true);
  assert.equal(response.__ccJson.success.structuredContent.code, "client_tool_invalid_arguments");
  assert.equal(response.__ccJson.success.structuredContent.executed, false);
  assert.deepEqual(response.__ccJson.success.structuredContent.errors.map((error) => error.path).sort(), ["project", "repo_path"]);
  assert.equal(session.currentRound, null);
  assert.equal(session.invalidToolCalls, 1);
  assert.doesNotMatch(output.text(), /tool_call/);
});

test("schema-invalid HTTP MCP calls use the same preflight and enum validation", async () => {
  const { session, output } = seedSession("invalid-http-mcp", "key", [{
    name: "todo",
    description: "todo",
    inputSchema: {
      type: "object",
      properties: { op: { type: "string", enum: ["init", "view"] } },
      required: ["op"],
      additionalProperties: false,
    },
  }]);
  const response = await mcpDispatch({
    jsonrpc: "2.0",
    id: 9,
    method: "tools/call",
    params: { name: "todo", arguments: { op: "create" } },
  }, session.id, DEFAULT_MCP_SERVER_KEY);
  assert.equal(response.result.isError, true);
  assert.equal(response.result.structuredContent.code, "client_tool_invalid_arguments");
  assert.equal(response.result.structuredContent.executed, false);
  assert.equal(response.result.structuredContent.errors[0].keyword, "enum");
  assert.equal(session.currentRound, null);
  assert.equal(session.invalidToolCalls, 1);
  assert.doesNotMatch(output.text(), /tool_call/);
});

test("repeated schema-invalid calls terminate the SDK run instead of spinning forever", async () => {
  const { session, output } = seedSession("invalid-call-loop", "key", [{
    name: "Required",
    inputSchema: { type: "object", required: ["value"] },
  }]);
  for (let i = 0; i < COMPOSER_MAX_INVALID_TOOL_CALLS; i++) {
    const response = await session.dispatchMcp({ toolName: "Required", args: {}, toolCallId: `invalid-${i}` });
    assert.equal(response.__ccJson.success.isError, true);
  }
  assert.equal(session.invalidToolCalls, COMPOSER_MAX_INVALID_TOOL_CALLS);
  assert.equal(session.loopTripped, true);
  assert.match(output.text(), /internal client-tool rejection bound/);
  assert.doesNotMatch(output.text(), /"type":"tool_call"/);
});

test("HTTP unknown tools and unmatched native retries share the same terminal rejection bound", async () => {
  const http = seedSession("unknown-http-loop", "key", advertised("Lookup"));
  for (let i = 0; i < COMPOSER_MAX_INVALID_TOOL_CALLS; i++) {
    const response = await mcpDispatch({
      jsonrpc: "2.0",
      id: i,
      method: "tools/call",
      params: { name: "Missing", arguments: {} },
    }, http.session.id, DEFAULT_MCP_SERVER_KEY);
    assert.equal(response.result.isError, true);
  }
  assert.equal(http.session.loopTripped, true);
  assert.equal(http.session.currentRound, null);
  assert.match(http.output.text(), /internal client-tool rejection bound/);

  const native = seedSession("unknown-native-loop", "key", []);
  for (let i = 0; i < COMPOSER_MAX_INVALID_TOOL_CALLS; i++) {
    const response = await native.session.dispatchUnary("readArgs", CC_CASES.readArgs, {
      toolCallId: `missing-native-${i}`,
      path: "/repo/a",
    });
    assert.ok(response.__ccJson.error);
  }
  assert.equal(native.session.loopTripped, true);
  assert.equal(native.session.currentRound, null);
  assert.match(native.output.text(), /internal client-tool rejection bound/);
});

test("repeated synthetic artifact retries terminate without ever reaching the harness", async () => {
  const artifact = "agent-tools/aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee.txt";
  const { session, output } = seedSession("synthetic-artifact-loop", "key", [{
    name: "write_file",
    inputSchema: { type: "object", properties: { path: { type: "string" }, content: { type: "string" } }, required: ["path"] },
  }]);
  for (let i = 0; i < COMPOSER_MAX_INVALID_TOOL_CALLS; i++) {
    const result = await session.openClientTool({
      source: "test",
      rawToolCallId: `artifact-${i}`,
      name: "write_file",
      input: { path: artifact, content: "result copy" },
      resultAdapter: (value) => value,
    });
    assert.equal(result.isError, true);
  }
  assert.equal(session.loopTripped, true);
  assert.equal(session.currentRound, null);
  assert.doesNotMatch(output.text(), /"type":"tool_call"/);
});

test("schema preflight runs after safe wrapper normalization and valid calls still open ToolRound", async () => {
  const { session } = seedSession("normalized-valid-mcp", "key", [{
    name: "Lookup",
    description: "lookup",
    inputSchema: {
      type: "object",
      properties: { q: { type: "string" } },
      required: ["q"],
      additionalProperties: false,
    },
  }]);
  const opened = await openTool(session, { input: { q: { text: "normalized" } } });
  assert.deepEqual(opened.call.input, { q: "normalized" });
  assert.equal(session.invalidToolCalls, 0);
  await session.cancel({ terminalReason: "test_cleanup", detail: "valid normalization test complete" });
  await assert.rejects(opened.promise, /test_cleanup/);
});

test("native read, write, and shell dispatch use the canonical client tool contracts", async () => {
  const cases = [
    {
      id: "native-read-contract",
      cas: "readArgs",
      spec: CC_CASES.readArgs,
      sdk: { path: "/repo/a.py", offset: 3, limit: 4, toolCallId: "native-read" },
      tool: "_read",
      schema: {
        type: "object",
        properties: { path: { type: "string" }, offset: { type: "number" }, limit: { type: "number" } },
        required: ["path"],
      },
      expected: { path: "/repo/a.py", offset: 3, limit: 4 },
    },
    {
      id: "native-write-contract",
      cas: "writeArgs",
      spec: CC_CASES.writeArgs,
      sdk: { path: "/repo/new.py", fileText: "print('ok')", toolCallId: "native-write" },
      tool: "_write",
      schema: {
        type: "object",
        properties: { path: { type: "string" }, content: { type: "string" } },
        required: ["path", "content"],
        additionalProperties: false,
      },
      expected: { path: "/repo/new.py", content: "print('ok')" },
    },
    {
      id: "native-shell-contract",
      cas: "shellArgs",
      spec: CC_CASES.shellArgs,
      sdk: { command: "pwd", workingDirectory: "/repo", toolCallId: "native-shell" },
      tool: "_bash",
      schema: {
        type: "object",
        properties: { command: { type: "string" }, cwd: { type: "string" } },
        required: ["command"],
        additionalProperties: false,
      },
      expected: { command: "pwd", cwd: "/repo" },
    },
  ];

  for (const item of cases) {
    const { session } = seedSession(item.id, "key", [{ name: item.tool, inputSchema: item.schema }]);
    const promise = session.dispatchUnary(item.cas, item.spec, item.sdk);
    promise.catch(() => {});
    await waitFor(() => session.currentRound && session.currentRound.fifo.length === 1, `${item.cas} ToolRound`);
    const call = session.currentRound.calls.get(session.currentRound.fifo[0]);
    assert.equal(call.name, item.tool);
    assert.deepEqual(call.input, item.expected);
    assert.equal(session.invalidToolCalls, 0);
    await session.cancel({ terminalReason: "test_cleanup", detail: `${item.cas} complete` });
    await assert.rejects(promise, /test_cleanup/);
    sessions.delete(session.id);
  }
});

test("native dispatch fails closed when no advertised client capability matches", async () => {
  const { session } = seedSession("native-no-match", "key", [{
    name: "delete_everything",
    inputSchema: { type: "object", properties: { path: { type: "string" } }, required: ["path"] },
  }]);
  const unary = await session.dispatchUnary("readArgs", CC_CASES.readArgs, { path: "/repo/a", toolCallId: "native-read" });
  assert.match(unary.__ccJson.error.error, /no advertised client tool safely matches/);
  assert.equal(session.currentRound, null);

  const chunks = [];
  for await (const chunk of session.dispatchStream("shellStreamArgs", CC_CASES.shellStreamArgs, {
    command: "pwd",
    toolCallId: "native-shell",
  })) chunks.push(chunk.__ccJson);
  assert.equal(chunks.at(-1).exit.code, 1);
  assert.equal(chunks.at(-1).exit.aborted, true);
  assert.equal(session.currentRound, null);
  sessions.delete(session.id);
});

test("native SDK capabilities route bidirectionally to common client spellings", async () => {
  const cases = [
    {
      id: "native-read-file",
      cas: "readArgs",
      spec: CC_CASES.readArgs,
      sdk: { path: "/repo/a", toolCallId: "read-file" },
      tool: "read_file",
      schema: { type: "object", properties: { file_path: { type: "string" } }, required: ["file_path"], additionalProperties: false },
      expected: { file_path: "/repo/a" },
    },
    {
      id: "native-write-file",
      cas: "writeArgs",
      spec: CC_CASES.writeArgs,
      sdk: { path: "/repo/a", fileText: "body", toolCallId: "write-file" },
      tool: "write_file",
      schema: {
        type: "object",
        properties: { file_path: { type: "string" }, file_text: { type: "string" } },
        required: ["file_path", "file_text"],
        additionalProperties: false,
      },
      expected: { file_path: "/repo/a", file_text: "body" },
    },
    {
      id: "native-terminal",
      cas: "shellArgs",
      spec: CC_CASES.shellArgs,
      sdk: { command: "pwd", workingDirectory: "/repo", toolCallId: "terminal" },
      tool: "run_terminal_cmd",
      schema: {
        type: "object",
        properties: { command: { type: "string" }, working_directory: { type: "string" } },
        required: ["command"],
        additionalProperties: false,
      },
      expected: { command: "pwd", working_directory: "/repo" },
    },
    {
      id: "native-remove-file",
      cas: "deleteArgs",
      spec: CC_CASES.deleteArgs,
      sdk: { path: "/repo/a", toolCallId: "remove-file" },
      tool: "remove_file",
      schema: { type: "object", properties: { file_path: { type: "string" } }, required: ["file_path"], additionalProperties: false },
      expected: { file_path: "/repo/a" },
    },
  ];
  for (const item of cases) {
    const { session } = seedSession(item.id, "key", [{ name: item.tool, inputSchema: item.schema }]);
    const pending = session.dispatchUnary(item.cas, item.spec, item.sdk);
    pending.catch(() => {});
    await waitFor(() => session.currentRound?.fifo.length === 1, `${item.tool} handoff`);
    const call = session.currentRound.calls.get(session.currentRound.fifo[0]);
    assert.equal(call.name, item.tool);
    assert.deepEqual(call.input, item.expected);
    await session.cancel({ terminalReason: "test_cleanup", detail: item.tool });
    await assert.rejects(pending, /test_cleanup/);
    sessions.delete(session.id);
  }
});

test("StrReplace and TodoWrite families translate into arbitrary client contracts before handoff", async () => {
  const { session } = seedSession("native-name-families", "key", [
    {
      name: "_edit",
      inputSchema: {
        type: "object",
        properties: {
          path: { type: "string" },
          old_string: { type: "string" },
          new_string: { type: "string" },
        },
        required: ["path", "old_string", "new_string"],
        additionalProperties: false,
      },
    },
    {
      name: "_todo",
      inputSchema: {
        type: "object",
        properties: {
          i: { type: "string", description: "concise intent" },
          op: { type: "string", enum: ["init"] },
          list: { type: "array", items: { type: "object" } },
        },
        required: ["i", "op", "list"],
        additionalProperties: false,
      },
    },
  ]);

  const editPromise = session.dispatchMcp({
    toolName: "StrReplace",
    toolCallId: "str-replace",
    args: { filePath: "/repo/a.py", oldString: "before", newString: "after" },
  });
  editPromise.catch(() => {});
  await waitFor(() => session.currentRound && session.currentRound.fifo.length === 1, "translated edit call");
  let call = session.currentRound.calls.get(session.currentRound.fifo[0]);
  assert.equal(call.name, "_edit");
  assert.deepEqual(call.input, { path: "/repo/a.py", old_string: "before", new_string: "after" });
  await session.cancel({ terminalReason: "test_cleanup", detail: "edit complete" });
  await assert.rejects(editPromise, /test_cleanup/);

  session.done = false;
  session.beginResponse(new MockResponse());
  const todoPromise = session.dispatchMcp({
    toolName: "TodoWrite",
    toolCallId: "todo-write",
    args: { op: { op: "init" }, list: { list: [{ task: "P0" }] } },
  });
  todoPromise.catch(() => {});
  await waitFor(() => session.currentRound && session.currentRound.fifo.length === 1, "translated todo call");
  call = session.currentRound.calls.get(session.currentRound.fifo[0]);
  assert.equal(call.name, "_todo");
  assert.deepEqual(call.input, { op: "init", list: [{ task: "P0" }] });
  assert.equal(session.invalidToolCalls, 0);
  await session.cancel({ terminalReason: "test_cleanup", detail: "todo complete" });
  await assert.rejects(todoPromise, /test_cleanup/);
});

test("replacing the advertised registry also replaces its schema validator", () => {
  const registry = new AdvertisedToolRegistry();
  registry.replace([{ name: "A", inputSchema: { type: "object", required: ["first"] } }]);
  const firstFailure = registry.validate("A", {});
  assert.equal(firstFailure.structuredContent.errors[0].path, "first");
  registry.replace([{ name: "A", inputSchema: { type: "object", required: ["second"] } }]);
  assert.equal(registry.validate("A", {}).structuredContent.errors[0].path, "second");
});

test("tool-name reconciliation is exact or unambiguous and never arbitrary single-tool routing", () => {
  assert.equal(reconcileExport(advertised("get_weather"), "get-weather"), "get_weather");
  assert.equal(reconcileExport(advertised("Bash"), "nanobanana_generate"), null);
  assert.equal(reconcileExport([{ name: "read_file" }, { name: "write_file" }], "file"), null);
});

test("signed continuation receipts are persisted before the live callback resolves", async () => {
  const { session } = seedSession("continue-live", "cursor-key");
  const events = [];
  const { call, promise, round } = await openTool(session, { adapter: (result) => { events.push("callback"); return result; } });
  const res = new MockResponse();
  const handling = handleContinue(request(), res, continuationBody([
    { toolCallId: call.wireId, content: "done", isError: false, structuredContent: { ok: true } },
  ]), "cursor-key");
  const result = await promise;
  await handling;
  assert.deepEqual(result.structuredContent, { ok: true });
  assert.deepEqual(events, ["callback"]);
  assert.equal(res.status, 200);
  assert.match(res.text(), /duplicate_or_concurrent/);
  const saved = journalRecord(round.route);
  assert.ok(saved.calls[0].resultHash);
  assert.deepEqual(saved.calls[0].receipt.result.structuredContent, { ok: true });
  assert.equal(saved.terminal.reason, "completed");
});

test("invalid continuation schemas are refused before a durable result receipt", async () => {
  const { session } = seedSession("invalid-continuation-schema", "cursor-key");
  const opened = await openTool(session);
  let callbackSettled = false;
  void opened.promise.then(() => { callbackSettled = true; }, () => { callbackSettled = true; });
  const res = new MockResponse();
  await handleContinue(request(), res, continuationBody([
    { toolCallId: opened.call.wireId, content: "must-not-commit" },
  ], {}, {
    tools: [{ name: "Broken", inputSchema: { type: "not-a-json-schema-type" } }],
  }), "cursor-key");
  assert.equal(res.status, 422);
  assert.match(res.text(), /invalid_advertised_tools/);
  assert.equal(callbackSettled, false);
  assert.equal(journalRecord(opened.round.route).calls[0].resultHash, null);
  await session.cancel({ terminalReason: "test_cleanup", detail: "invalid continuation test complete" });
  await assert.rejects(opened.promise, /test_cleanup/);
});

test("whole-batch token validation is atomic: one tampered id commits nothing", async () => {
  const { session } = seedSession("atomic", "cursor-key");
  const first = await openTool(session, { rawId: "one", awaiting: false });
  // Open a second call directly in the same collecting round before awaiting state.
  const secondPromise = session.openClientTool({ source: "test", rawToolCallId: "two", name: "Lookup", input: { q: "two" }, resultAdapter: (x) => x });
  secondPromise.catch(() => {});
  await waitFor(() => first.round.fifo.length === 2 && first.round.calls.get(first.round.fifo[1]).handedAt != null, "second handoff");
  if (session.flushTimer) { clearTimeout(session.flushTimer); session.flushTimer = null; }
  const second = first.round.calls.get(first.round.fifo[1]);
  first.round.markAwaitingResults();
  const tampered = second.wireId.slice(0, -1) + (second.wireId.endsWith("A") ? "B" : "A");
  const res = new MockResponse();
  await handleContinue(request(), res, continuationBody([
    { toolCallId: first.call.wireId, content: "valid" },
    { toolCallId: tampered, content: "invalid" },
  ]), "cursor-key");
  assert.equal(res.status, 400);
  assert.equal(first.round.calls.get(first.call.wireId).resultHash, null);
  assert.equal(first.round.calls.get(second.wireId).resultHash, null);
  first.round.terminalize("client_cancelled", "test cleanup");
  await assert.rejects(first.promise);
  await assert.rejects(secondPromise);
});

test("a continuation batch spanning two signed routes is rejected before either mutates", async () => {
  const a = seedSession("route-a", "cursor-key").session;
  const openA = await openTool(a, { rawId: "a" });
  const b = seedSession("route-b", "cursor-key").session;
  const openB = await openTool(b, { rawId: "b" });
  const res = new MockResponse();
  await handleContinue(request(), res, continuationBody([
    { toolCallId: openA.call.wireId, content: "A" },
    { toolCallId: openB.call.wireId, content: "B" },
  ]), "cursor-key");
  assert.equal(res.status, 409);
  assert.equal(openA.call.resultHash, null);
  assert.equal(openB.call.resultHash, null);
  openA.round.terminalize("client_cancelled", "cleanup");
  openB.round.terminalize("client_cancelled", "cleanup");
  await assert.rejects(openA.promise);
  await assert.rejects(openB.promise);
});

test("signed round tenant fingerprint prevents cross-credential result injection", async () => {
  const { session } = seedSession("tenant", "key-a");
  const opened = await openTool(session);
  const res = new MockResponse();
  await handleContinue(request(), res, continuationBody([{ toolCallId: opened.call.wireId, content: "stolen" }]), "key-b");
  assert.equal(res.status, 403);
  assert.equal(opened.call.resultHash, null);
  opened.round.terminalize("client_cancelled", "cleanup");
  await assert.rejects(opened.promise);
});

test("multi-tenant continuation refuses a round with a missing tenant fingerprint", () => {
  assert.equal(continuationTenantMismatch({ tenantFingerprint: "" }, "cursor-key", true), true);
  assert.equal(continuationTenantMismatch({}, "cursor-key", true), true);
  assert.equal(continuationTenantMismatch({ tenantFingerprint: "" }, "cursor-key", false), false);
  assert.equal(continuationTenantMismatch({ tenantFingerprint: keyFingerprint("cursor-key") }, "cursor-key", true), false);
  assert.equal(continuationTenantMismatch({ tenantFingerprint: keyFingerprint("other-key") }, "cursor-key", true), true);
});

test("result-before-handoff is rejected without creating a receipt", async () => {
  const session = new Session("not-handed", "key");
  session.clientEnv = { workspaceUnknown: true };
  const round = session.ensureToolRound();
  const call = round.openCall({ source: "test", rawToolCallId: "raw", name: "Lookup", input: {}, callback: { resolve() {}, reject() {} } });
  round.markAwaitingResults();
  const res = new MockResponse();
  await handleContinue(request(), res, continuationBody([{ toolCallId: call.wireId, content: "early" }]), "key");
  assert.equal(res.status, 409);
  assert.equal(round.calls.get(call.wireId).receipt, null);
});

test("restart without bounded history durably receipts the result then refuses faithful recovery", async () => {
  const { session } = seedSession("restart-refuse", "cursor-key");
  const opened = await openTool(session);
  opened.round.clearTimer(opened.call);
  liveToolRounds.delete(opened.round.route);
  sessions.delete(session.id);
  const res = new MockResponse();
  await handleContinue(request(), res, continuationBody([{ toolCallId: opened.call.wireId, content: "executed" }]), "cursor-key");
  assert.equal(res.status, 410);
  assert.equal(res.json().error.code, "round_lost");
  const saved = journalRecord(opened.round.route);
  assert.equal(saved.calls[0].receipt.result.content, "executed");
  assert.equal(saved.terminal.reason, "restart_lost");
  assert.equal(saved.recovery.decision, "refused");
});

test("restart accepts a registered call receipt when the handoff journal write was the crash boundary", async () => {
  const session = new Session("restart-handoff-window", "cursor-key");
  session.clientEnv = { workspaceUnknown: true };
  const round = session.ensureToolRound();
  const call = round.openCall({ source: "test", rawToolCallId: "registered", name: "Lookup", input: { q: "x" } });
  assert.equal(call.handedAt, null);
  liveToolRounds.delete(round.route);
  sessions.delete(session.id);

  const res = new MockResponse();
  await handleContinue(request(), res, continuationBody([{ toolCallId: call.wireId, content: "executed after frame delivery" }]), "cursor-key");
  assert.equal(res.status, 410);
  const saved = journalRecord(round.route);
  assert.equal(saved.calls[0].receipt.result.content, "executed after frame delivery");
  assert.equal(saved.terminal.reason, "restart_lost");
});

test("late result after a run-error terminal is receipted but never acknowledged as success", async () => {
  const { session } = seedSession("late-after-error", "cursor-key");
  const opened = await openTool(session);
  opened.round.terminalize("run_error", "upstream failed");
  sessions.delete(session.id);
  liveToolRounds.delete(opened.round.route);
  await assert.rejects(opened.promise, /run_error/);

  const res = new MockResponse();
  await handleContinue(request(), res, continuationBody([{ toolCallId: opened.call.wireId, content: "late local result" }]), "cursor-key");
  assert.equal(res.status, 410);
  assert.doesNotMatch(res.text(), /already_applied|end_turn/);
  const saved = journalRecord(opened.round.route);
  assert.equal(saved.calls[0].receipt.result.content, "late local result");
  assert.equal(saved.terminal.reason, "run_error");
});

test("result-adapter failure leaves the durable receipt and terminalizes the round", async () => {
  const { session } = seedSession("adapter-failure", "cursor-key");
  const opened = await openTool(session, { adapter: () => { throw new Error("adapter exploded"); } });
  const res = new MockResponse();
  await handleContinue(request(), res, continuationBody([{ toolCallId: opened.call.wireId, content: "durable first" }]), "cursor-key");
  await assert.rejects(opened.promise, /adapter exploded/);
  assert.equal(res.status, 500);
  const saved = journalRecord(opened.round.route);
  assert.equal(saved.calls[0].receipt.result.content, "durable first");
  assert.equal(saved.terminal.reason, "run_error");
});

test("restart recovery preserves ids, errors, structured data, images, and trailing text", async () => {
  const { session } = seedSession("recovery-shape", "key");
  const opened = await openTool(session);
  const input = {
    type: "tool_results",
    history: "bounded prior conversation",
    userText: "then continue",
    results: [{
      toolCallId: opened.call.wireId,
      content: { message: "failed" },
      isError: true,
      structuredContent: { code: "E_FAIL" },
      images: [{ data: "QUJD", mimeType: "image/png" }, { url: "https://example.test/x.png" }],
    }],
  };
  const recovery = buildRestartRecoveryInput(opened.round, input);
  assert.equal(recovery.type, "user");
  assert.match(recovery.text, new RegExp(opened.call.wireId));
  assert.match(recovery.text, /E_FAIL/);
  assert.match(recovery.text, /then continue/);
  assert.equal(recovery.images.length, 2);
  opened.round.terminalize("client_cancelled", "cleanup");
  await assert.rejects(opened.promise);
});

test("identical late retry is acknowledged from its durable terminal receipt; conflict is rejected", async () => {
  const { session } = seedSession("late", "cursor-key");
  const opened = await openTool(session);
  const result = { toolCallId: opened.call.wireId, content: "once", isError: false };
  const first = new MockResponse();
  await Promise.all([handleContinue(request(), first, continuationBody([result]), "cursor-key"), opened.promise]);
  sessions.delete(session.id);
  liveToolRounds.delete(opened.round.route);

  const duplicate = new MockResponse();
  await handleContinue(request(), duplicate, continuationBody([result]), "cursor-key");
  assert.equal(duplicate.status, 200);
  assert.match(duplicate.text(), /already_applied/);

  const conflict = new MockResponse();
  await handleContinue(request(), conflict, continuationBody([{ ...result, content: "different" }]), "cursor-key");
  assert.equal(conflict.status, 409);
  assert.equal(conflict.json().error.code, "result_conflict");
});

test("a completed restart recovery durably acknowledges an identical lost-response retry", async () => {
  const { session } = seedSession("recovery-idempotent", "cursor-key");
  const opened = await openTool(session);
  opened.round.terminalize("shutdown", "bridge restarted");
  await assert.rejects(opened.promise, /shutdown/);
  liveToolRounds.delete(opened.round.route);
  sessions.delete(session.id);

  const result = { toolCallId: opened.call.wireId, content: "executed once" };
  opened.round.commitResults([result], { allowTerminalReceipt: true });
  opened.round.recordRecovery({
    at: Date.now(),
    decision: "faithful_reseed",
    replacementAgentId: "replacement-agent",
    resultHashes: [opened.round.calls.get(opened.call.wireId).resultHash],
  });

  const replacement = new Session(opened.round.sessionId, "cursor-key");
  replacement.agentId = "replacement-agent";
  replacement.recoverySourceRound = opened.round;
  replacement.streamedText = "recovered answer";
  const output = new MockResponse();
  replacement.beginResponse(output);
  sessions.set(replacement.id, replacement);
  replacement.onRunComplete({ status: "finished", result: "recovered answer" });

  const saved = journalRecord(opened.round.route);
  assert.equal(saved.recovery.decision, "completed");
  assert.ok(saved.recovery.completedAt);

  const duplicate = new MockResponse();
  await handleContinue(request(), duplicate, continuationBody([result], { history: "bounded prior conversation" }), "cursor-key");
  assert.equal(duplicate.status, 200);
  assert.match(duplicate.text(), /already_recovered/);
});

test("a failed restart recovery is journaled and releases its replacement session for retry", async () => {
  const { session } = seedSession("recovery-failed", "cursor-key");
  const opened = await openTool(session);
  opened.round.terminalize("shutdown", "bridge restarted");
  await assert.rejects(opened.promise, /shutdown/);
  liveToolRounds.delete(opened.round.route);
  sessions.delete(session.id);

  const result = { toolCallId: opened.call.wireId, content: "executed once" };
  opened.round.commitResults([result], { allowTerminalReceipt: true });
  opened.round.recordRecovery({ at: Date.now(), decision: "faithful_reseed", replacementAgentId: "failed-agent" });

  const replacement = new Session(opened.round.sessionId, "cursor-key");
  replacement.agentId = "failed-agent";
  replacement.recoverySourceRound = opened.round;
  const output = new MockResponse();
  replacement.beginResponse(output);
  sessions.set(replacement.id, replacement);
  replacement.onRunComplete({ status: "error", error: "replacement failed" });

  assert.equal(journalRecord(opened.round.route).recovery.decision, "failed");
  assert.equal(sessions.has(replacement.id), false);
  assert.match(output.text(), /"stop_reason":"error"/);
});

test("cancel terminal-resolves handed and registered calls exactly once", async () => {
  const { session } = seedSession("cancel", "key");
  const first = await openTool(session, { rawId: "first", awaiting: false });
  session.activeRes = null;
  session.responseWriter = null;
  const secondPromise = session.openClientTool({ source: "test", rawToolCallId: "second", name: "Lookup", input: {}, resultAdapter: (x) => x });
  secondPromise.catch(() => {});
  await waitFor(() => first.round.fifo.length === 2, "registered second call");
  const second = first.round.calls.get(first.round.fifo[1]);
  assert.equal(second.handedAt, null);
  await session.cancel({ terminalReason: "interrupted", detail: "user redirect" });
  assert.equal(first.round.state, "TERMINAL");
  assert.ok([...first.round.calls.values()].every((call) => call.state === "TERMINAL"));
  await assert.rejects(first.promise, /interrupted/);
  await assert.rejects(secondPromise, /interrupted/);
  assert.equal(first.round.terminal.reason, "interrupted");
});

test("a slow tail tool emitted after turn_end stays journaled in the same round and hands on the next response", async () => {
  const { session } = seedSession("slow-tail", "key");
  const first = await openTool(session, { rawId: "first" });
  first.round.batch = [];
  session.activeRes = null;
  session.responseWriter = null;

  const secondPromise = session.openClientTool({ source: "test", rawToolCallId: "tail", name: "Lookup", input: { q: "tail" }, resultAdapter: (value) => value });
  secondPromise.catch(() => {});
  await waitFor(() => first.round.fifo.length === 2, "journaled slow tail");
  const second = first.round.calls.get(first.round.fifo[1]);
  assert.equal(second.handedAt, null);
  assert.equal(second.wireId.split("_")[1], first.call.wireId.split("_")[1]);

  const nextResponse = new MockResponse();
  session.beginResponse(nextResponse);
  assert.equal(session.flushJournaledCalls(), true);
  await waitFor(() => second.handedAt != null, "slow tail handoff on next response");
  assert.match(nextResponse.text(), new RegExp(second.wireId));
  first.round.terminalize("client_cancelled", "cleanup");
  await assert.rejects(first.promise);
  await assert.rejects(secondPromise);
});

test("new user input interrupts an awaiting round without relying on a deleted Session pending map", async () => {
  const { session } = seedSession("interrupt-path", "key");
  const opened = await openTool(session);
  session.run = { async cancel() {} };
  session.tail = new Promise(() => {});
  const res = new MockResponse();
  await handleTurn(request(), res, neutralBody({ sessionId: session.id, input: { type: "user", text: "new direction" } }), "key");
  assert.equal(res.status, 200);
  assert.equal(opened.round.terminal.reason, "interrupted");
  await assert.rejects(opened.promise, /interrupted/);
  sessions.delete(session.id);
});

test("a finished SDK run with an unresolved callback emits an error terminal, never end_turn", async () => {
  const { session, output } = seedSession("unfinished-callback", "key");
  const opened = await openTool(session);
  session.onRunComplete({ status: "finished", result: "" });
  await assert.rejects(opened.promise, /run_error/);
  assert.match(output.text(), /"stop_reason":"error"/);
  assert.doesNotMatch(output.text(), /"stop_reason":"end_turn"/);
  assert.equal(opened.round.terminal.reason, "run_error");
});

test("response disconnect after tool handoff terminal-resolves the round as transport_error", async () => {
  const { session, output } = seedSession("disconnect-after-handoff", "key");
  const opened = await openTool(session);
  output.emit("close");
  await assert.rejects(opened.promise, /transport_error/);
  assert.equal(opened.round.terminal.reason, "transport_error");
  assert.equal(opened.round.pendingCount, 0);
});

test("transport loss before tool-call handoff terminal-resolves a registered call without fake delivery", async () => {
  const session = new Session("disconnect-before-handoff", "key");
  session.clientEnv = { workspaceUnknown: true };
  session.advertise = advertised();
  const promise = session.openClientTool({
    source: "test",
    rawToolCallId: "not-handed",
    name: "Lookup",
    input: { q: "x" },
    resultAdapter: (value) => value,
  });
  promise.catch(() => {});
  await waitFor(() => session.currentRound?.fifo.length === 1, "registered unhanded call");
  const call = session.currentRound.calls.get(session.currentRound.fifo[0]);
  assert.equal(call.handedAt, null);
  await session.cancel({ terminalReason: "transport_error", detail: "client disconnected before handoff" });
  await assert.rejects(promise, /transport_error/);
  assert.equal(call.handedAt, null);
  assert.equal(session.currentRound.terminal.reason, "transport_error");
  assert.equal(session.currentRound.pendingCount, 0);
});

test("shutdown cancellation terminal-resolves every open callback", async () => {
  const { session } = seedSession("shutdown-cancel", "key");
  const opened = await openTool(session);
  await session.cancel({ terminalReason: "shutdown", detail: "test shutdown" });
  await assert.rejects(opened.promise, /shutdown/);
  assert.equal(opened.round.terminal.reason, "shutdown");
  assert.equal(opened.round.pendingCount, 0);
});

test("HTTP MCP tools/call uses ToolRound and preserves isError, structuredContent, and image output", async () => {
  const { session } = seedSession("mcp-http", "key");
  const responsePromise = mcpDispatch({ jsonrpc: "2.0", id: 7, method: "tools/call", params: { name: "Lookup", arguments: { q: "x" } } }, session.id, DEFAULT_MCP_SERVER_KEY);
  await waitFor(() => session.currentRound?.fifo.length === 1 && session.currentRound.calls.get(session.currentRound.fifo[0]).handedAt, "HTTP MCP handoff");
  if (session.flushTimer) { clearTimeout(session.flushTimer); session.flushTimer = null; }
  const round = session.currentRound;
  const call = round.calls.get(round.fifo[0]);
  assert.equal(call.source, "http-mcp");
  round.markAwaitingResults();
  round.applyResults([{
    toolCallId: call.wireId,
    content: "failed",
    isError: true,
    structuredContent: { code: "E_LOOKUP" },
    images: [{ data: "QUJD", mimeType: "image/png" }],
  }]);
  const response = await responsePromise;
  assert.equal(response.result.isError, true);
  assert.deepEqual(response.result.structuredContent, { code: "E_LOOKUP" });
  assert.deepEqual(response.result.content, [
    { type: "image", data: "QUJD", mimeType: "image/png" },
    { type: "text", text: "failed" },
  ]);
});

test("HTTP MCP calls are server-scoped and equal JSON-RPC ids cannot collide across servers", async () => {
  const tools = [
    { name: "mcp__alpha__one", inputSchema: { type: "object", properties: { q: { type: "string" } }, required: ["q"] } },
    { name: "mcp__beta__two", inputSchema: { type: "object", properties: { q: { type: "string" } }, required: ["q"] } },
  ];
  const { session } = seedSession("mcp-server-scope", "key", tools);
  session.mcpServerKeys = [DEFAULT_MCP_SERVER_KEY, "alpha", "beta"];

  const wrongServer = await mcpDispatch({
    jsonrpc: "2.0", id: 9, method: "tools/call", params: { name: "mcp__beta__two", arguments: { q: "x" } },
  }, session.id, "alpha");
  assert.equal(wrongServer.result.isError, true);
  assert.match(wrongServer.result.content[0].text, /not available/);
  assert.equal(session.currentRound, null);

  const alpha = mcpDispatch({
    jsonrpc: "2.0", id: 1, method: "tools/call", params: { name: "mcp__alpha__one", arguments: { q: "a" } },
  }, session.id, "alpha");
  const beta = mcpDispatch({
    jsonrpc: "2.0", id: 1, method: "tools/call", params: { name: "mcp__beta__two", arguments: { q: "b" } },
  }, session.id, "beta");
  await waitFor(() => session.currentRound?.fifo.length === 2
    && session.currentRound.fifo.every((wireId) => session.currentRound.calls.get(wireId).handedAt), "two scoped MCP calls");
  const round = session.currentRound;
  const calls = round.fifo.map((wireId) => round.calls.get(wireId));
  assert.notEqual(calls[0].rawIdHash, calls[1].rawIdHash);
  assert.deepEqual(calls.map((call) => call.name).sort(), ["mcp__alpha__one", "mcp__beta__two"]);
  if (session.flushTimer) { clearTimeout(session.flushTimer); session.flushTimer = null; }
  round.markAwaitingResults();
  round.applyResults(calls.map((call) => ({ toolCallId: call.wireId, content: call.name, isError: false })));
  const responses = await Promise.all([alpha, beta]);
  assert.deepEqual(responses.map((response) => response.id), [1, 1]);
});

test("JSON-RPC tool batches dispatch concurrently and preserve response order", async () => {
  const { session } = seedSession("mcp-batch", "key", [
    { name: "A", inputSchema: { type: "object", properties: { q: { type: "string" } }, required: ["q"] } },
    { name: "B", inputSchema: { type: "object", properties: { q: { type: "string" } }, required: ["q"] } },
  ]);
  const pending = dispatchMcpBatch([
    { jsonrpc: "2.0", id: 11, method: "tools/call", params: { name: "A", arguments: { q: "a" } } },
    { jsonrpc: "2.0", id: 12, method: "tools/call", params: { name: "B", arguments: { q: "b" } } },
  ], session.id, DEFAULT_MCP_SERVER_KEY);
  await waitFor(() => session.currentRound?.fifo.length === 2
    && session.currentRound.fifo.every((wireId) => session.currentRound.calls.get(wireId).handedAt), "parallel batch handoff");
  const round = session.currentRound;
  if (session.flushTimer) { clearTimeout(session.flushTimer); session.flushTimer = null; }
  round.markAwaitingResults();
  round.applyResults(round.fifo.map((wireId) => ({ toolCallId: wireId, content: "ok", isError: false })));
  const responses = await pending;
  assert.deepEqual(responses.map((response) => response.id), [11, 12]);
});

test("patched MCP dispatch uses the same ToolRound adapter and result contract", async () => {
  const { session } = seedSession("mcp-patched", "key");
  const responsePromise = session.dispatchMcp({ toolName: "Lookup", args: { q: "x" }, toolCallId: "sdk-mcp" });
  await waitFor(() => session.currentRound?.fifo.length === 1 && session.currentRound.calls.get(session.currentRound.fifo[0]).handedAt, "patched MCP handoff");
  if (session.flushTimer) { clearTimeout(session.flushTimer); session.flushTimer = null; }
  const round = session.currentRound;
  const call = round.calls.get(round.fifo[0]);
  assert.equal(call.source, "patched-mcp");
  round.markAwaitingResults();
  round.applyResults([{ toolCallId: call.wireId, content: "failed", isError: true, structuredContent: { code: "E_LOOKUP" } }]);
  const response = await responsePromise;
  assert.deepEqual(response.__ccJson, mcpDispatchResult("failed", true, null, { code: "E_LOOKUP" }));
});

test("MCP missing session and unknown tool are typed errors, never fake success", async () => {
  const missing = await mcpDispatch({ jsonrpc: "2.0", id: 1, method: "tools/call", params: { name: "Lookup", arguments: {} } }, "gone", DEFAULT_MCP_SERVER_KEY);
  assert.equal(missing.result.isError, true);
  const { session } = seedSession("unknown-tool", "key", [{ name: "Read" }, { name: "Bash" }]);
  const unknown = await mcpDispatch({ jsonrpc: "2.0", id: 2, method: "tools/call", params: { name: "DestroyEverything", arguments: {} } }, session.id, DEFAULT_MCP_SERVER_KEY);
  assert.equal(unknown.result.isError, true);
  assert.match(unknown.result.content[0].text, /not available/);
});

test("MCP tools/list is derived live from the registry with object schemas", async () => {
  const { session } = seedSession("list", "key", [{ name: "A" }, { name: "B", inputSchema: { type: "object", required: ["q"] } }]);
  const listed = await mcpDispatch({ jsonrpc: "2.0", id: 3, method: "tools/list" }, session.id, DEFAULT_MCP_SERVER_KEY);
  assert.deepEqual(listed.result.tools.map((tool) => tool.name), ["A", "B"]);
  assert.deepEqual(listed.result.tools[0].inputSchema, { type: "object" });
  assert.deepEqual(mcpToolsForServer(session, DEFAULT_MCP_SERVER_KEY).map((tool) => tool.name), ["A", "B"]);
});

test("Grok 4.5 model aliases preserve slow default and map effort", () => {
  assert.deepEqual(composerModelSelection("cursor-grok-4.5"), { id: "grok-4.5", params: [{ id: "fast", value: "false" }, { id: "effort", value: "high" }] });
  assert.deepEqual(composerModelSelection("cursor-grok-4.5-fast-low"), { id: "grok-4.5", params: [{ id: "fast", value: "true" }, { id: "effort", value: "low" }] });
  assert.deepEqual(composerModelSelection("cursor-grok-4.5-xhigh"), { id: "grok-4.5", params: [{ id: "fast", value: "false" }, { id: "effort", value: "high" }] });
});

test("tool-choice gating does not widen the advertised set or fake native success", () => {
  const tools = advertised("Lookup");
  assert.deepEqual(effectiveAdvertise(tools, "none"), []);
  assert.deepEqual(effectiveAdvertise(tools, "specific:Missing"), []);
  assert.equal(nativeToolBlockedByChoice("none"), true);
  assert.equal(nativeToolBlockedByChoice("specific:Lookup"), true);
  assert.equal(nativeToolBlockedByChoice("auto"), false);
  const blocked = blockedNativeResult("readArgs", { path: "/x" }, "");
  assert.ok(blocked.__ccJson.error);
  assert.match(JSON.stringify(blocked), /disabled/);
  assert.match(constraintInstructions({ toolChoice: "required" }), /must call/i);
});

test("synthetic agent-tools artifacts are blocked before ToolRound across native, MCP, and shell seams", async () => {
  const artifact = "agent-tools/fdc5389e-988d-4ef4-8d1b-f31037f28f8a.txt";
  assert.equal(syntheticAgentArtifactRequest("Write", { file_path: artifact }), true);
  assert.equal(syntheticAgentArtifactRequest("mcp__client-tools__Read", { path: `/tmp/${artifact}` }), true);
  assert.equal(syntheticAgentArtifactRequest("Bash", { command: `head -c 2000 ${artifact}` }), true);
  assert.equal(syntheticAgentArtifactRequest("write_file", { file_path: artifact }), true);
  assert.equal(syntheticAgentArtifactRequest("read-file", { path: artifact }), true);
  assert.equal(syntheticAgentArtifactRequest("run_terminal_cmd", { command: `head ${artifact}` }), true);
  assert.equal(syntheticAgentArtifactRequest("Write", { file_path: "agent-tools/notes.txt" }), false);
  assert.equal(syntheticAgentArtifactRequest("Write", { file_path: "/home/me/repo/normal.txt" }), false);
  assert.equal(syntheticAgentArtifactFailure("Lookup", { path: artifact }), null);

  const { session, output } = seedSession("synthetic-artifact", "key", [
    { name: "Write", inputSchema: { type: "object", properties: { path: { type: "string" }, content: { type: "string" } } } },
    { name: "Read", inputSchema: { type: "object", properties: { path: { type: "string" } } } },
    { name: "Bash", inputSchema: { type: "object", properties: { command: { type: "string" } } } },
  ]);

  const direct = await session.openClientTool({ source: "test", rawToolCallId: "synthetic-direct", name: "Write", input: { path: artifact, content: "large result" }, resultAdapter: (value) => value });
  assert.equal(direct.isError, true);
  assert.match(direct.content, /already available|use it directly/i);
  assert.equal(session.currentRound, null, "a rejected internal artifact must not allocate a durable client ToolRound");
  assert.doesNotMatch(output.text(), /tool_call/);

  const native = await session.dispatchUnary("writeArgs", CC_CASES.writeArgs, { toolCallId: "native-write", path: artifact, fileText: "large result" });
  assert.ok(native.__ccJson.error, "native write receives the real typed filesystem error variant");
  assert.match(JSON.stringify(native), /agent-tools artifact handoff is disabled/);
  assert.equal(session.currentRound, null);

  const mcp = await session.dispatchMcp({ toolCallId: "mcp-write", toolName: "Write", args: { path: artifact, content: "large result" } });
  assert.equal(mcp.__ccJson.success.isError, true, "an MCP-aliased Write cannot bypass the same structural gate");
  assert.match(JSON.stringify(mcp), /already available|use it directly/i);
  assert.equal(session.currentRound, null);

  const chunks = [];
  for await (const chunk of session.dispatchStream("shellStreamArgs", CC_CASES.shellStreamArgs, { toolCallId: "native-shell", command: `ls ${artifact}` })) chunks.push(chunk.__ccJson);
  assert.equal(chunks.at(-1).exit.code, 1);
  assert.equal(chunks.at(-1).exit.aborted, true);
  assert.equal(session.currentRound, null);
});

test("SDK-dispatch preflight blocks reserved artifacts before generic client-tool routing", async () => {
  const artifact = "agent-tools/15428c99-00fa-4575-b194-10adac970a34.txt";
  const { session } = seedSession("synthetic-sdk-preflight", "key", [{
    name: "Write",
    inputSchema: { type: "object", properties: { path: { type: "string" }, content: { type: "string" } } },
  }]);
  session.resetSyntheticArtifactGuard("");

  const unary = await blockedSyntheticNativeExecIfNeeded(
    { session },
    "writeArgs",
    { toolCallId: "native-artifact", path: artifact, fileText: "internal" },
    false,
  );
  assert.ok(unary.__ccJson.error);
  assert.match(JSON.stringify(unary), /agent-tools artifact handoff is disabled/);
  assert.equal(session.currentRound, null);

  const stream = blockedSyntheticNativeExecIfNeeded(
    { session },
    "shellStreamArgs",
    { toolCallId: "native-artifact-readback", command: `head ${artifact}` },
    true,
  );
  const chunks = [];
  for await (const chunk of stream) chunks.push(chunk.__ccJson);
  assert.equal(chunks.at(-1).exit.code, 1);
  assert.equal(session.currentRound, null);
});

test("result-aware artifact guard blocks renamed copies and their readback, then resets on a real user turn", async () => {
  const largeMcpResult = `${"architecture-node: value\n".repeat(1800)}tail`;
  const renamedPath = "/tmp/cursor-result-cache.txt";
  const { session, output } = seedSession("synthetic-result-aware", "key", [
    { name: "Write", inputSchema: { type: "object", properties: { path: { type: "string" }, content: { type: "string" } } } },
    { name: "Read", inputSchema: { type: "object", properties: { path: { type: "string" } } } },
    { name: "Bash", inputSchema: { type: "object", properties: { command: { type: "string" } } } },
  ]);
  session.resetSyntheticArtifactGuard("summarize the repository");
  assert.equal(session.rememberLargeMcpResult(largeMcpResult, "codebase-memory-mcp"), true);

  const write = await session.openClientTool({
    source: "patched-unary",
    rawToolCallId: "renamed-write",
    name: "Write",
    input: { path: renamedPath, content: largeMcpResult },
    resultAdapter: (value) => value,
  });
  assert.equal(write.isError, true);
  assert.equal(write.structuredContent.reason, "copied_large_mcp_result");
  assert.equal(session.currentRound, null);
  assert.doesNotMatch(output.text(), /tool_call/);

  const read = await session.openClientTool({
    source: "patched-unary",
    rawToolCallId: "renamed-read",
    name: "Read",
    input: { path: renamedPath },
    resultAdapter: (value) => value,
  });
  assert.equal(read.structuredContent.reason, "rejected_artifact_followup");
  assert.match(read.content, /already been rejected 2 times/);

  const shell = await session.openClientTool({
    source: "patched-stream",
    rawToolCallId: "renamed-shell",
    name: "Bash",
    input: { command: `wc -c ${renamedPath}` },
    resultAdapter: (value) => value,
  });
  assert.equal(shell.structuredContent.reason, "rejected_artifact_followup");
  assert.equal(session.currentRound, null);

  session.resetSyntheticArtifactGuard("now do something unrelated");
  assert.equal(session.syntheticArtifactDecision("Read", { path: renamedPath }), null);
  assert.equal(session.syntheticArtifactDecision("Write", { path: "/tmp/other.txt", content: largeMcpResult }), null);
});

test("an explicitly user-requested artifact path is never swallowed by the internal guard", () => {
  const artifact = "agent-tools/367167f6-68b2-4941-84dd-e7e40affbb43.txt";
  const largeMcpResult = "x".repeat(40_000);
  const session = new Session("synthetic-user-override", "key");
  session.resetSyntheticArtifactGuard(`Write the exact MCP result to ${artifact}`);
  session.rememberLargeMcpResult(largeMcpResult, "Lookup");
  assert.equal(session.syntheticArtifactDecision("Write", { path: artifact, content: largeMcpResult }), null);
  assert.equal(session.syntheticArtifactDecision("Bash", { command: `head ${artifact}` }), null);
});

test("the neutral manifest never teaches internal artifact or wrapper behavior", () => {
  const external = toolManifest([{ name: "mcp__codebase-memory-mcp__get_architecture", description: "graph" }]);
  assert.doesNotMatch(external, /agent-tools|GetMcpTools|CallMcpTool|MCP server|MCP transport/i);
  const harnessOnly = toolManifest([{ name: "Write", description: "write a user-requested file" }]);
  assert.doesNotMatch(harnessOnly, /agent-tools/);
});

test("native result builders preserve failures and completeness metadata", () => {
  const failed = CC_CASES.readArgs.buildResult("permission denied", { path: "/x" }, true, { cwd: "" });
  assert.ok(failed.error);
  assert.equal(failed.success, undefined);
  const bounded = buildReadSuccess("one\ntwo", { path: "/x", limit: 2 });
  assert.equal(bounded.success.truncated, true);
  const structured = buildReadSuccess({ content: "one", truncated: false, rangeApplied: true, totalLines: 9, fileSize: 99 }, { path: "/x" });
  assert.equal(structured.success.totalLines, "9");
  const write = buildWriteSuccess({ fileContentAfterWrite: "actual", linesCreated: 1 }, { path: "/x", fileText: "requested", returnFileContentAfterWrite: true });
  assert.equal(write.success.fileContentAfterWrite, "actual");
  const ompStatus = "[/repo/a.py#A1B2]\nSuccessfully wrote 9 bytes to /repo/a.py";
  const statusWrite = buildWriteSuccess(ompStatus, { path: "/repo/a.py", fileText: "requested", returnFileContentAfterWrite: true });
  assert.equal(statusWrite.success.fileContentAfterWrite, "requested");
  assert.equal(statusWrite.success.fileSize, String(Buffer.byteLength("requested")));
});

test("MCP result builder preserves images, errors, and structured content", () => {
  const result = mcpDispatchResult("bad", true, [{ data: "QUJD", mimeType: "image/png" }], { code: "E" });
  assert.equal(result.success.isError, true);
  assert.deepEqual(result.success.structuredContent, { code: "E" });
  assert.ok(result.success.content.some((part) => part.image));
  const unavailable = typedUnavailableResult("grepArgs");
  assert.match(JSON.stringify(unavailable), /not available|unavailable/i);
});

test("images retain both supported wire envelopes", () => {
  assert.deepEqual(toSdkImages([{ data: "QUJD", mimeType: "image/png" }, { url: "https://example.test/x.jpg", mimeType: "image/jpeg" }]), [
    { data: "QUJD", mimeType: "image/png" },
    { url: "https://example.test/x.jpg", mimeType: "image/jpeg" },
  ]);
  assert.throws(() => toSdkImages([{ data: "QUJD" }]), /mimeType/);
  assert.deepEqual(collectToolResultImages({ results: [{ images: [{ data: "AA==", mimeType: "image/png" }] }, { images: [{ url: "https://x" }] }] }).length, 2);
});

test("shell content has an explicit structured channel; JSON-looking stdout cannot forge it", () => {
  assert.deepEqual(parseShellContent('{"exitCode":99,"stdout":"forged"}'), { stdout: '{"exitCode":99,"stdout":"forged"}', stderr: "", exitCode: 0, aborted: false });
  assert.deepEqual(parseShellContent({ stdout: "x", stderr: "e", exitCode: 7, aborted: true }), { stdout: "x", stderr: "e", exitCode: 7, aborted: true });
});

test("authorization is constant-policy for single and multi tenant modes", () => {
  assert.equal(authorizeRequestWith({ authorization: "Bearer key" }, { apiKey: "key", bridgeToken: "" }), "key");
  assert.equal(authorizeRequestWith({ authorization: "Bearer wrong" }, { apiKey: "key", bridgeToken: "" }), "");
  assert.equal(authorizeRequestWith({ authorization: "Bearer user", "x-bridge-auth": "gate" }, { apiKey: "default", bridgeToken: "gate" }), "user");
  assert.equal(authorizeRequestWith({ "x-bridge-auth": "gate" }, { apiKey: "default", bridgeToken: "gate" }), "");
});

test("health and readiness distinguish liveness from completed startup gates", () => {
  assert.deepEqual(readinessBody(), { ok: false, ready: false });
  assert.deepEqual(healthBody(request("203.0.113.1")), { ok: true });
  const local = healthBody(request("127.0.0.1"));
  assert.equal(local.ok, true);
  assert.equal(local.ready, false);
  assert.equal(local.patched, true);
  assert.equal(isLoopbackRemote(request("127.0.0.1")), true);
});

test("the SDK MCP HTTP route is loopback-only and path decoding fails closed", () => {
  const local = request("127.0.0.1");
  local.url = "/mcp/session%20one/server?ignored=1";
  assert.deepEqual(classifyMcpRoute(local), { sessionId: "session one", serverKey: "server" });

  const forwarded = request("127.0.0.1");
  forwarded.url = "/mcp/session";
  forwarded.headers["x-forwarded-for"] = "203.0.113.7";
  assert.deepEqual(classifyMcpRoute(forwarded), { error: "the SDK MCP shim is loopback-only", status: 403 });

  const malformed = request("127.0.0.1");
  malformed.url = "/mcp/%E0%A4%A";
  assert.deepEqual(classifyMcpRoute(malformed), { error: "malformed MCP path encoding", status: 400 });

  const extra = request("127.0.0.1");
  extra.url = "/mcp/session/server/extra";
  assert.deepEqual(classifyMcpRoute(extra), { error: "malformed MCP path", status: 400 });
});

test("non-loopback bind requires an authenticated or explicit insecure deployment", () => {
  assert.equal(bindHostIsLoopback("127.0.0.1"), true);
  assert.equal(bindHostIsLoopback("0.0.0.0"), false);
  assert.equal(validateBindHost("0.0.0.0", false, false).ok, false);
  assert.equal(validateBindHost("0.0.0.0", true, false).ok, true);
  assert.match(validateBindHost("0.0.0.0", false, true).warn, /plaintext/);
});

test("utility helpers preserve strict types and bounded output", () => {
  assert.deepEqual(wrapToolInput("raw"), { input: "raw" });
  assert.deepEqual(wrapToolInput({ q: "x" }), { q: "x" });
  assert.match(truncateLiveToolResult("abcdefgh", 4), /abcd[\s\S]*truncated by proxy/);
  assert.equal(isConversationTooLong("ERROR_CONVERSATION_TOO_LONG"), true);
  assert.equal(keyFingerprint("a").length, 64);
  const name = "TEST_CCT_ENV_INT";
  process.env[name] = "17";
  assert.equal(envInt(name, 3, { min: 1 }), 17);
  process.env[name] = "10m";
  assert.equal(envInt(name, 3, { min: 1 }), 3);
  delete process.env[name];
});
