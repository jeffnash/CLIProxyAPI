import assert from "node:assert/strict";
import test from "node:test";

import {
  ToolContractRegistry,
  ToolContractNormalizationError,
  acceptanceSchemaFor,
  clientToolFamily,
  normalizeToolArguments,
  normalizeToolResultEnvelope,
  resolveClientToolName,
  toolContractCacheStats,
} from "./tool-contract-adapter.mjs";

const intentProperty = { type: "string", description: "concise intent" };

function registryFor(name, inputSchema) {
  const registry = new ToolContractRegistry();
  registry.replace([{ name, inputSchema }]);
  return registry;
}

test("schema adapter repairs OMP todo array wrappers before validation", () => {
  const schema = {
    type: "object",
    properties: {
      i: intentProperty,
      op: { type: "string", enum: ["init", "append"] },
      list: {
        type: "array",
        items: {
          type: "object",
          properties: { task: { type: "string" }, status: { type: "string" } },
          required: ["task"],
          additionalProperties: false,
        },
      },
    },
    required: ["i", "op", "list"],
    additionalProperties: false,
  };
  const registry = registryFor("_todo", schema);
  const normalized = registry.normalize("_todo", {
    i: { text: "Initialize remediation packets" },
    op: { op: "init" },
    list: { list: [{ task: { text: "P0" }, status: "pending" }] },
  });
  assert.deepEqual(normalized.value, {
    i: "Initialize remediation packets",
    op: "init",
    list: [{ task: "P0", status: "pending" }],
  });
  assert.equal(registry.validate("_todo", normalized.value, normalized.transforms), null);
  assert.ok(normalized.transforms.some((entry) => entry.path === "list" && entry.toType === "array"));
});

test("array adapters accept same-key, items, value, JSON, and serialized wrappers", () => {
  const schema = {
    type: "object",
    properties: { values: { type: "array", items: { type: "number" } } },
    required: ["values"],
    additionalProperties: false,
  };
  for (const wrapped of [
    { values: [1, 2] },
    { items: [1, 2] },
    { value: [1, 2] },
    { 0: 1, 1: 2 },
    "[1,2]",
    { toJSON: () => [1, 2] },
  ]) {
    assert.deepEqual(normalizeToolArguments({ values: wrapped }, schema).value, { values: [1, 2] });
  }
});

test("wrapped numeric and boolean scalars are coerced only when the schema requires those types", () => {
  const schema = {
    type: "object",
    properties: { limit: { type: "integer" }, recursive: { type: "boolean" }, text: { type: "string" } },
    required: ["limit", "recursive", "text"],
  };
  assert.deepEqual(normalizeToolArguments({
    limit: { value: "12" },
    recursive: { value: "true" },
    text: { value: "12" },
  }, schema).value, { limit: 12, recursive: true, text: "12" });
});

test("optional empty SDK placeholders are omitted but required values still fail validation", () => {
  const schema = {
    type: "object",
    properties: { query: { type: "string" }, limit: { type: "number" }, required_count: { type: "number" } },
    required: ["query", "required_count"],
    additionalProperties: false,
  };
  const registry = registryFor("search", schema);
  const normalized = registry.normalize("search", { query: "x", limit: {}, required_count: {} });
  assert.deepEqual(normalized.value, { query: "x", required_count: {} });
  assert.equal(registry.validate("search", normalized.value).structuredContent.errors[0].path, "required_count");
});

test("legitimate object values are never unwrapped merely because they contain wrapper-like keys", () => {
  const schema = {
    type: "object",
    properties: {
      payload: {
        type: "object",
        properties: { value: { type: "string" }, items: { type: "array" } },
        required: ["value"],
      },
    },
    required: ["payload"],
  };
  const payload = { value: "real", items: ["also-real"] };
  assert.deepEqual(normalizeToolArguments({ payload }, schema).value, { payload });
});

test("native argument keys map to unique client schema keys without overwriting exact client input", () => {
  const schema = {
    type: "object",
    properties: {
      path: { type: "string" },
      old_string: { type: "string" },
      new_string: { type: "string" },
    },
    required: ["path", "old_string", "new_string"],
    additionalProperties: false,
  };
  const translated = normalizeToolArguments({
    filePath: "/repo/a.py",
    oldString: "before",
    newString: "after",
  }, schema);
  assert.deepEqual(translated.value, {
    path: "/repo/a.py",
    old_string: "before",
    new_string: "after",
  });
  assert.equal(translated.transforms.filter((entry) => entry.kind === "rename-key").length, 3);

  const exactWins = normalizeToolArguments({ path: "/correct", filePath: "/wrong", old_string: "a", new_string: "b" }, schema);
  assert.equal(exactWins.value.path, "/correct");
  assert.equal(Object.hasOwn(exactWins.value, "filePath"), false);
  assert.ok(exactWins.transforms.some((entry) => entry.kind === "discard-conflicting-alias" && entry.source === "filePath"));
  const equalAliases = normalizeToolArguments({ path: "/same", filePath: { value: "/same" }, old_string: "a", new_string: "b" }, schema);
  assert.equal(equalAliases.value.path, "/same");
  assert.equal(Object.hasOwn(equalAliases.value, "filePath"), false);
  assert.ok(equalAliases.transforms.some((entry) => entry.kind === "deduplicate-alias" && entry.source === "filePath"));
});

test("exact OMP grep fields win over redundant Cursor aliases without weakening the client schema", () => {
  const schema = {
    type: "object",
    properties: {
      i: { type: "string", description: "concise intent" },
      pattern: { type: "string" },
      path: { type: "string" },
      selector: { type: "string" },
      case: { type: "boolean" },
      gitignore: { type: "boolean" },
      skip: { type: ["number", "null"] },
    },
    required: ["i", "pattern"],
    additionalProperties: false,
  };
  const registry = registryFor("_grep", schema);
  const normalized = registry.normalize("_grep", {
    i: "Find turn lifecycle symbols",
    pattern: "_active_turns\\[",
    query: "different native search wording",
    path: "omnigent/runner/app.py",
    filePath: "/incorrect/native/path",
    head_limit: 100,
    output_mode: "content",
    limit: 20,
  });
  assert.deepEqual(normalized.value, {
    i: "Find turn lifecycle symbols",
    pattern: "_active_turns\\[",
    path: "omnigent/runner/app.py",
  });
  assert.equal(registry.validate("_grep", normalized.value, normalized.transforms), null);
  assert.deepEqual(
    normalized.transforms.filter((entry) => entry.kind === "discard-conflicting-alias").map((entry) => entry.source).sort(),
    ["filePath", "query"],
  );
  assert.deepEqual(
    normalized.transforms.filter((entry) => entry.kind === "strip-presentation").map((entry) => entry.path).sort(),
    ["head_limit", "limit", "output_mode"],
  );

  const clientOwnedLimit = registryFor("_grep", {
    ...schema,
    properties: { ...schema.properties, limit: { type: "integer" } },
  }).normalize("_grep", { i: "Limit results", pattern: "x", limit: 3 });
  assert.equal(clientOwnedLimit.value.limit, 3, "a field declared by the client is never stripped");

  assert.throws(
    () => normalizeToolArguments({ query: "one", regex: "two" }, schema),
    (error) => error instanceof ToolContractNormalizationError && error.code === "ambiguous_alias",
    "two aliases without an exact client field remain unsafe to guess",
  );
});

test("prototype-named client fields remain own JSON properties and inherited values never satisfy schemas", () => {
  const schema = JSON.parse(`{"type":"object","properties":{"__proto__":{"type":"string"},"constructor":{"type":"string"}},"required":["__proto__","constructor"],"additionalProperties":false}`);
  const registry = registryFor("prototype-fields", schema);
  const input = JSON.parse(`{"__proto__":"safe","constructor":"also-safe"}`);
  const normalized = registry.normalize("prototype-fields", input);
  assert.equal(Object.hasOwn(normalized.value, "__proto__"), true);
  assert.equal(normalized.value.__proto__, "safe");
  assert.equal(normalized.value.constructor, "also-safe");
  assert.equal(registry.validate("prototype-fields", normalized.value), null);

  const inheritedOnly = Object.create(Object.fromEntries([
    ["__proto__", "inherited"],
    ["constructor", "inherited"],
  ]));
  const failure = registry.validate("prototype-fields", inheritedOnly);
  assert.deepEqual(failure.structuredContent.errors.map((error) => error.path).sort(), ["__proto__", "constructor"]);

  const composed = registryFor("prototype-allof", {
    type: "object",
    allOf: [{
      properties: JSON.parse(`{"__proto__":{"type":"string"}}`),
      required: ["__proto__"],
    }],
    additionalProperties: true,
  });
  const composedValue = composed.normalize("prototype-allof", JSON.parse(`{"__proto__":{"stringValue":"safe"}}`));
  assert.equal(Object.hasOwn(composedValue.value, "__proto__"), true);
  assert.equal(composedValue.value.__proto__, "safe");
  assert.equal(composed.validate("prototype-allof", composedValue.value), null);
});

test("native read/write/shell keys adapt to Claude-style and other client spellings", () => {
  assert.deepEqual(normalizeToolArguments(
    { path: "/repo/a.py" },
    { type: "object", properties: { file_path: { type: "string" } }, required: ["file_path"], additionalProperties: false },
  ).value, { file_path: "/repo/a.py" });
  assert.deepEqual(normalizeToolArguments(
    { path: "/repo/a.py", content: "x" },
    {
      type: "object",
      properties: { file_path: { type: "string" }, file_text: { type: "string" } },
      required: ["file_path", "file_text"],
      additionalProperties: false,
    },
  ).value, { file_path: "/repo/a.py", file_text: "x" });
  assert.deepEqual(normalizeToolArguments(
    { command: "pwd", workingDirectory: "/repo" },
    {
      type: "object",
      properties: { command: { type: "string" }, cwd: { type: "string" } },
      required: ["command"],
      additionalProperties: false,
    },
  ).value, { command: "pwd", cwd: "/repo" });
});

test("argument repair never maps workspace locations into search queries", () => {
  const schema = {
    type: "object",
    properties: { pattern: { type: "string" } },
    required: ["pattern"],
    additionalProperties: false,
  };
  const normalized = normalizeToolArguments({ cwd: "/repo" }, schema);
  assert.deepEqual(normalized.value, { cwd: "/repo" });
  assert.equal(Object.hasOwn(normalized.value, "pattern"), false);
});

test("undeclared arguments envelopes flatten into the declared client contract", () => {
  const schema = {
    type: "object",
    properties: { query: { type: "string" }, limit: { type: "number" } },
    required: ["query"],
    additionalProperties: false,
  };
  const normalized = normalizeToolArguments({ arguments: { query: "needle", limit: 5 } }, schema);
  assert.deepEqual(normalized.value, { query: "needle", limit: 5 });
  assert.equal(normalized.transforms[0].kind, "flatten-envelope");
});

test("nested and JSON-string argument envelopes flatten recursively while declared wrapper fields stay intact", () => {
  const schema = {
    type: "object",
    properties: { file_path: { type: "string" } },
    required: ["file_path"],
    additionalProperties: false,
  };
  assert.deepEqual(normalizeToolArguments({ arguments: { input: { path: "/repo/a" } } }, schema).value, { file_path: "/repo/a" });
  assert.deepEqual(normalizeToolArguments({ params: JSON.stringify({ path: "/repo/b" }) }, schema).value, { file_path: "/repo/b" });

  const declared = {
    type: "object",
    properties: { input: { type: "object", properties: { path: { type: "string" } }, required: ["path"] } },
    required: ["input"],
  };
  assert.deepEqual(normalizeToolArguments({ input: { path: "/repo/c" } }, declared).value, { input: { path: "/repo/c" } });
});

test("local $defs and allOf schemas normalize with the same structure Ajv validates", () => {
  const schema = {
    $schema: "https://json-schema.org/draft/2020-12/schema",
    $ref: "#/$defs/input",
    $defs: {
      input: {
        allOf: [
          {
            type: "object",
            properties: { file_path: { type: "string" } },
            required: ["file_path"],
          },
          {
            type: "object",
            properties: {
              values: { $ref: "#/$defs/values" },
              i: { type: "string", description: "concise intent" },
            },
            required: ["values", "i"],
          },
        ],
      },
      values: { type: "array", items: { type: "number" } },
    },
  };
  const registry = registryFor("ref-tool", schema);
  const normalized = registry.normalize("ref-tool", {
    path: { text: "/repo/a" },
    values: { items: [{ value: "1" }, 2] },
  });
  assert.deepEqual(normalized.value, { file_path: "/repo/a", values: [1, 2] });
  assert.equal(registry.validate("ref-tool", normalized.value), null);
});

test("mixed direct and allOf properties participate in normalization", () => {
  const schema = {
    type: "object",
    properties: { label: { type: "string" } },
    required: ["label"],
    allOf: [{
      type: "object",
      properties: { file_path: { type: "string" } },
      required: ["file_path"],
    }],
  };
  const registry = registryFor("mixed-allof", schema);
  const normalized = registry.normalize("mixed-allof", { label: { text: "x" }, path: { value: "/repo/a" } });
  assert.deepEqual(normalized.value, { label: "x", file_path: "/repo/a" });
  assert.equal(registry.validate("mixed-allof", normalized.value), null);
});

test("union and enum wrappers are selected by unique schema validation, not current object kind", () => {
  const union = {
    type: "object",
    properties: {
      choice: {
        oneOf: [
          { type: "string" },
          { type: "object", properties: { kind: { const: "record" } }, required: ["kind"], additionalProperties: false },
        ],
      },
      mode: { enum: ["fast", "safe"] },
    },
    required: ["choice", "mode"],
  };
  assert.deepEqual(normalizeToolArguments({ choice: { value: "text" }, mode: { value: "safe" } }, union).value, {
    choice: "text",
    mode: "safe",
  });
  const record = { kind: "record" };
  assert.deepEqual(normalizeToolArguments({ choice: record, mode: "fast" }, union).value.choice, record);
});

test("anyOf branch-only required fields are not treated as universally required placeholders", () => {
  const schema = {
    type: "object",
    properties: { a: { type: "string" }, b: { type: "number" } },
    anyOf: [{ required: ["a"] }, { required: ["b"] }],
  };
  assert.deepEqual(normalizeToolArguments({ a: "selected", b: {} }, schema).value, { a: "selected" });
});

test("bare SDK values map to a schema's sole root property", () => {
  const schema = {
    type: "object",
    properties: { query: { type: "string" } },
    required: ["query"],
    additionalProperties: false,
  };
  assert.deepEqual(normalizeToolArguments("needle", schema).value, { query: "needle" });
});

test("OMP intent decoration is optional for execution while semantic fields remain required", () => {
  const schema = {
    type: "object",
    properties: { i: intentProperty, repo_path: { type: "string" } },
    required: ["i", "repo_path"],
    additionalProperties: false,
  };
  const registry = registryFor("mcp__memory__status", schema);
  assert.equal(registry.validate("mcp__memory__status", { repo_path: "/repo" }), null);
  assert.equal(registry.validate("mcp__memory__status", { i: { sdk: "wrapper" }, repo_path: "/repo" }), null);
  const failure = registry.validate("mcp__memory__status", {});
  assert.deepEqual(failure.structuredContent.errors.map((error) => error.path), ["repo_path"]);
  assert.deepEqual(acceptanceSchemaFor(schema).required, ["repo_path"]);
});

test("a real tool parameter named i is not relaxed without the decoration marker", () => {
  const schema = {
    type: "object",
    properties: { i: { type: "string", description: "matrix row identifier" } },
    required: ["i"],
  };
  const registry = registryFor("real-i", schema);
  assert.equal(registry.validate("real-i", {}).structuredContent.errors[0].path, "i");
});

test("description-pruned intent is relaxed only when the whole inventory corroborates it", () => {
  const schema = (field) => ({
    type: "object",
    properties: {
      i: { type: "string" },
      [field]: { type: "string" },
    },
    required: [field, "i"],
    additionalProperties: false,
  });
  const registry = new ToolContractRegistry();
  registry.replace([
    { name: "read", description: "", inputSchema: schema("path") },
    { name: "search", description: "", inputSchema: schema("query") },
  ]);
  assert.equal(registry.validate("read", { path: "/repo/a" }), null);
  assert.equal(registry.validate("search", { query: "needle" }), null);

  const single = registryFor("real-pruned-i", schema("value"));
  assert.equal(single.validate("real-pruned-i", { value: "semantic" }).structuredContent.errors[0].path, "i");
});

test("description-pruned intent detection ignores semantically irrelevant property and required ordering", () => {
  const registry = new ToolContractRegistry();
  registry.replace([
    {
      name: "_write",
      description: "",
      inputSchema: {
        type: "object",
        // This is the order produced by Go's sorted map-key encoder.
        properties: { command: { type: "string" }, i: { type: "string" } },
        required: ["i", "command"],
        additionalProperties: false,
      },
    },
    {
      name: "_read",
      description: "",
      inputSchema: {
        type: "object",
        properties: { file_path: { type: "string" }, i: { type: "string" } },
        required: ["file_path", "i"],
        additionalProperties: false,
      },
    },
  ]);
  assert.equal(registry.validate("_write", { command: "pwd" }), null);
  assert.equal(registry.validate("_read", { file_path: "/repo/a" }), null);
});

test("explicit client-decoration extension works for any harness and field name", () => {
  const schema = {
    type: "object",
    properties: {
      trace_label: { type: "string", "x-cliproxy-client-decoration": true },
      value: { type: "number" },
    },
    required: ["trace_label", "value"],
  };
  const registry = registryFor("generic", schema);
  assert.equal(registry.validate("generic", { value: 2 }), null);
});

test("tool-name adapter covers native read/write/edit/shell/todo families and refuses ambiguity", () => {
  const tools = ["_read", "_write", "_edit", "_bash", "_todo", "mcp__memory__search"].map((name) => ({ name }));
  assert.equal(resolveClientToolName("Read", tools), "_read");
  assert.equal(resolveClientToolName("WriteFile", tools), "_write");
  assert.equal(resolveClientToolName("StrReplace", tools), "_edit");
  assert.equal(resolveClientToolName("run_terminal_cmd", tools), "_bash");
  assert.equal(resolveClientToolName("TodoWrite", tools), "_todo");
  assert.equal(resolveClientToolName("mcp__memory__search", tools), "mcp__memory__search");
  assert.equal(resolveClientToolName("DeleteEverything", tools), null);
  assert.equal(resolveClientToolName("StrReplace", [{ name: "edit" }, { name: "_edit" }]), null);
  assert.equal(resolveClientToolName("FileSearch", [{ name: "glob" }, { name: "grep" }]), null);
  assert.equal(resolveClientToolName("read_everything", [{ name: "delete_everything" }]), null);
  assert.equal(resolveClientToolName("terminal", [{ name: "bash" }, { name: "shell" }]), null);
  assert.equal(resolveClientToolName("native-shell", [{ name: "Execute", aliases: ["native-shell"] }]), "Execute");
  assert.equal(resolveClientToolName("Read", [{ name: "Read" }, { name: "RunCommand", aliases: ["Read"] }]), "Read");
  assert.equal(resolveClientToolName("read", [{ name: "read_file" }]), "read_file");
  assert.equal(resolveClientToolName("write", [{ name: "write_file" }]), "write_file");
  assert.equal(resolveClientToolName("shell", [{ name: "run_terminal_cmd" }]), "run_terminal_cmd");
  assert.equal(resolveClientToolName("edit", [{ name: "str_replace" }]), "str_replace");
  assert.equal(resolveClientToolName("todo", [{ name: "todo_write" }]), "todo_write");
  assert.equal(resolveClientToolName("delete", [{ name: "remove_file" }]), "remove_file");
  assert.equal(clientToolFamily("write_file"), "write");
  assert.equal(clientToolFamily("read-file"), "read");
  assert.equal(clientToolFamily("run_terminal_cmd"), "shell");
});

test("result adapter keeps errors, structured content, and both image envelopes losslessly", () => {
  const result = normalizeToolResultEnvelope(
    { rows: 2 },
    true,
    [{ url: "https://example.test/a.png" }, { data: "base64", mimeType: "image/png" }],
    { code: "failed" },
  );
  assert.deepEqual(result.content, { rows: 2 });
  assert.equal(result.isError, true);
  assert.equal(result.inlineImages.length, 1);
  assert.equal(result.urlImages.length, 1);
  assert.equal(result.images.length, 2);
  assert.equal(result.images[0].url, "https://example.test/a.png", "mixed image envelopes retain wire order");
  assert.deepEqual(result.structuredContent, { code: "failed" });
  assert.equal(result.structuredContentPresent, true);
  assert.throws(
    () => normalizeToolResultEnvelope("bad", false, [{ data: "broken" }], undefined),
    (error) => error.code === "invalid_result_image",
  );
});

test("protobuf Value and JSON-encoded scalar wrappers normalize structurally", () => {
  const schema = {
    type: "object",
    properties: {
      op: { type: "string", enum: ["get"] },
      count: { type: "integer" },
      flags: { type: "array", items: { type: "boolean" } },
      config: { type: "object", additionalProperties: { type: "string" } },
    },
    required: ["op", "count", "flags", "config"],
    additionalProperties: false,
  };
  const normalized = normalizeToolArguments({
    op: { kind: { case: "stringValue", value: "get" } },
    count: { numberValue: 2 },
    flags: { listValue: { values: [{ boolValue: true }, { boolValue: false }] } },
    config: { structValue: { fields: { mode: { stringValue: "safe" } } } },
  }, schema);
  assert.deepEqual(normalized.value, { op: "get", count: 2, flags: [true, false], config: { mode: "safe" } });
  assert.deepEqual(normalizeToolArguments({ op: '"get"' }, {
    type: "object", properties: { op: { type: "string", enum: ["get"] } }, required: ["op"], additionalProperties: false,
  }).value, { op: "get" });
});

test("numeric coercion accepts JSON numbers but rejects JavaScript-only spellings", () => {
  const schema = { type: "object", properties: { count: { type: "number" } }, required: ["count"] };
  assert.deepEqual(normalizeToolArguments({ count: { value: "1.25e2" } }, schema).value, { count: 125 });
  assert.deepEqual(normalizeToolArguments({ count: { value: "0x10" } }, schema).value, { count: { value: "0x10" } });

  const integerSchema = { type: "object", properties: { count: { type: "integer" } }, required: ["count"] };
  assert.deepEqual(normalizeToolArguments({ count: { value: "9007199254740991" } }, integerSchema).value,
    { count: 9007199254740991 });
  for (const unsafe of ["9007199254740992", "9007199254740993", "-9007199254740992", "1e20"]) {
    assert.throws(
      () => normalizeToolArguments({ count: { value: unsafe } }, integerSchema),
      (error) => error instanceof ToolContractNormalizationError && error.code === "unsafe_integer",
      `unsafe integer ${unsafe} must not be rounded into an executable client call`,
    );
  }
  assert.throws(
    () => normalizeToolArguments({ count: 9007199254740992 }, integerSchema),
    (error) => error instanceof ToolContractNormalizationError && error.code === "unsafe_integer" && error.path === "arguments.count",
  );
});

test("root null is never fabricated into an empty invocation", () => {
  const objectSchema = { type: "object", properties: {}, additionalProperties: false };
  assert.equal(normalizeToolArguments(null, objectSchema).value, null);
  assert.deepEqual(normalizeToolArguments(undefined, objectSchema).value, {});
});

test("pattern and additional-property schemas normalize dynamic values recursively", () => {
  const schema = {
    type: "object",
    patternProperties: { "^n_": { type: "integer" } },
    additionalProperties: { type: "boolean" },
  };
  assert.deepEqual(normalizeToolArguments({ n_count: { value: "3" }, enabled: { value: "true" } }, schema).value,
    { n_count: 3, enabled: true });
});

test("only explicitly annotated advisory fields are stripped", () => {
  const schema = {
    type: "object",
    properties: {
      query: { type: "string" },
      limit: { type: "integer", "x-cliproxy-advisory-ignored": true },
    },
    required: ["query", "limit"],
    additionalProperties: false,
  };
  const registry = registryFor("search", schema);
  const normalized = registry.normalize("search", { query: "x", limit: 100 });
  assert.deepEqual(normalized.value, { query: "x" });
  assert.equal(registry.validate("search", normalized.value, normalized.transforms), null);
  assert.ok(normalized.transforms.some((entry) => entry.kind === "strip-advisory"));
});

test("conflicting nested and outer envelope fields fail closed", () => {
  const schema = { type: "object", properties: { query: { type: "string" } }, required: ["query"], additionalProperties: false };
  assert.throws(
    () => normalizeToolArguments({ arguments: { query: "inner" }, query: "outer" }, schema),
    (error) => error.code === "ambiguous_envelope",
  );
});

test("schema validator caches remain byte- and entry-bounded under dynamic MCP churn", () => {
  const registry = new ToolContractRegistry();
  for (let generation = 0; generation < 96; generation++) {
    registry.replace([{
      name: `dynamic_${generation}`,
      inputSchema: {
        type: "object",
        properties: {
          value: { type: "string", description: `${generation}:${"x".repeat(32 << 10)}` },
        },
        required: ["value"],
        additionalProperties: false,
      },
    }]);
  }
  const stats = toolContractCacheStats();
  for (const cache of Object.values(stats)) {
    assert.ok(cache.entries <= cache.maxEntries, JSON.stringify(cache));
    assert.ok(cache.bytes <= cache.maxBytes, JSON.stringify(cache));
  }
});
