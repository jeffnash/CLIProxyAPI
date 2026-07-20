import { createHash } from "node:crypto";
import Ajv from "ajv";
import Ajv2019 from "ajv/dist/2019.js";
import Ajv2020 from "ajv/dist/2020.js";
import AjvDraft04 from "ajv-draft-04";

// The bridge is a protocol adapter between two independently evolving tool
// runtimes:
//
//   harness JSON Schema <-> Cursor SDK/model tool representation
//
// Keep every compatibility transform here. The client-facing descriptor and
// emitted call remain authoritative; SDK-specific wrappers and native naming
// habits are normalized before validation and before a ToolRound is opened.

const TOOL_SCHEMA_VALIDATOR_CACHE_MAX_ENTRIES = 512;
const TOOL_SCHEMA_VALIDATOR_CACHE_MAX_BYTES = 16 << 20;

class ByteBoundedLRU {
  constructor(maxEntries, maxBytes) {
    this.maxEntries = maxEntries;
    this.maxBytes = maxBytes;
    this.bytes = 0;
    this.values = new Map();
  }
  get(key) {
    const entry = this.values.get(key);
    if (!entry) return undefined;
    this.values.delete(key);
    this.values.set(key, entry);
    return entry.value;
  }
  set(key, value, bytes) {
    const size = Math.max(1024, Math.min(Number.MAX_SAFE_INTEGER, Number(bytes) || 0));
    const prior = this.values.get(key);
    if (prior) {
      this.bytes -= prior.bytes;
      this.values.delete(key);
    }
    if (size > this.maxBytes) return value;
    this.values.set(key, { bytes: size, value });
    this.bytes += size;
    while (this.values.size > this.maxEntries || this.bytes > this.maxBytes) {
      const oldest = this.values.keys().next().value;
      const evicted = this.values.get(oldest);
      this.values.delete(oldest);
      this.bytes -= evicted.bytes;
    }
    return value;
  }
  stats() { return { bytes: this.bytes, entries: this.values.size, maxBytes: this.maxBytes, maxEntries: this.maxEntries }; }
}

const validatorCache = new ByteBoundedLRU(TOOL_SCHEMA_VALIDATOR_CACHE_MAX_ENTRIES, TOOL_SCHEMA_VALIDATOR_CACHE_MAX_BYTES);
const normalizationValidatorCache = new ByteBoundedLRU(TOOL_SCHEMA_VALIDATOR_CACHE_MAX_ENTRIES, TOOL_SCHEMA_VALIDATOR_CACHE_MAX_BYTES);

function schemaCacheIdentity(mode, schema) {
  const encoded = stableJSON(schema);
  return {
    bytes: Buffer.byteLength(encoded, "utf8") * 8,
    key: `${mode}:${createHash("sha256").update(encoded).digest("hex")}`,
  };
}

export function toolContractCacheStats() {
  return {
    contracts: validatorCache.stats(),
    normalization: normalizationValidatorCache.stats(),
  };
}

export class ToolContractNormalizationError extends Error {
  constructor(code, path, message) {
    super(message);
    this.name = "ToolContractNormalizationError";
    this.code = code;
    this.path = path || "arguments";
  }
}

const ENVELOPE_KEYS = ["arguments", "args", "input", "parameters", "params", "targeting"];
const GENERIC_WRAPPER_KEYS = ["value", "input", "content", "data", "payload"];
const ARRAY_WRAPPER_KEYS = ["items", "list", "values", "array"];
const OBJECT_WRAPPER_KEYS = ["object", "record", "map"];
const STRING_WRAPPER_KEYS = ["text", "string"];
// Cursor/model adapters sometimes preserve a scalar variant as a tagged
// object. These keys are considered only when the advertised schema requires
// a primitive and the extracted value is the one unique schema-valid choice.
// A client-owned object field therefore never loses a legitimate `kind`,
// `selected`, or `enumValue` property.
const SCALAR_VARIANT_WRAPPER_KEYS = ["kind", "selected", "selection", "enum", "enumValue", "scalar", "scalarValue"];
const PROTOBUF_JSON_VALUE_TYPES = new Set([
  "google.protobuf.Value",
  "google.protobuf.Struct",
  "google.protobuf.ListValue",
]);
const PROTOCOL_DECODE_MAX_DEPTH = 32;
const PROTOCOL_DECODE_MAX_NODES = 4096;

const ARGUMENT_ALIASES = Object.freeze({
  absolutepath: [["filePath", "path", "file", "filename"], 80],
  cmd: [["command", "cmd", "script"], 95],
  commandline: [["command", "cmd", "script"], 80],
  content: [["content", "fileText", "text", "newString"], 95],
  contents: [["content", "newString", "text"], 70],
  cwd: [["cwd", "workingDirectory", "workdir"], 90],
  directory: [["directory", "path"], 60],
  filetext: [["content", "text", "newString"], 95],
  filepath: [["filePath", "path", "file", "filename"], 90],
  filename: [["filePath", "path", "file", "filename"], 75],
  glob: [["pattern", "glob", "include"], 85],
  globpattern: [["pattern", "glob", "include"], 95],
  include: [["include", "pattern", "glob"], 70],
  items: [["todos", "items", "tasks", "list"], 70],
  list: [["list", "todos", "items", "tasks"], 75],
  newcontents: [["content", "newString", "replacement", "text"], 85],
  newstring: [["newString", "replacement", "content"], 95],
  newtext: [["newString", "replacement", "content", "text"], 85],
  oldcontents: [["oldString", "old", "search", "text"], 80],
  oldstring: [["oldString", "old", "search"], 95],
  oldtext: [["oldString", "old", "search", "text"], 85],
  path: [["filePath", "path", "file", "filename"], 75],
  pattern: [["pattern", "query", "regex", "search"], 80],
  prompt: [["prompt", "description", "instructions", "query"], 80],
  query: [["query", "pattern", "search", "prompt"], 80],
  regex: [["pattern", "regex", "query"], 75],
  replacement: [["newString", "replacement", "content"], 85],
  script: [["command", "script", "cmd"], 75],
  search: [["pattern", "query", "oldString", "search"], 70],
  searchstring: [["pattern", "query", "oldString", "search"], 80],
  targetdirectory: [["directory", "path"], 70],
  targetfile: [["filePath", "path", "file", "filename"], 90],
  targeting: [["path", "directory", "filePath"], 45],
  tasks: [["todos", "tasks", "items", "list"], 75],
  todo: [["todos", "items", "tasks", "list"], 70],
  url: [["url", "uri", "href"], 90],
  workingdirectory: [["workdir", "cwd"], 95],
});

const TOOL_NAME_FAMILIES = Object.freeze([
  ["read", "readfile", "openfile"],
  ["write", "writefile", "createfile"],
  ["edit", "editfile", "replacefile", "searchreplace", "strreplace", "applypatch"],
  ["bash", "shell", "terminal", "runterminalcmd", "runcommand", "exec", "execcommand"],
  ["todo", "todowrite"],
  ["delete", "deletefile", "removefile", "unlinkfile"],
  ["glob", "fileglob", "findfiles"],
  ["grep", "searchfiles"],
  ["ls", "list"],
]);

const CROSS_FAMILY_TOOL_ALIASES = Object.freeze({
  filesearch: ["glob", "grep"],
});

const TOOL_FAMILY_KINDS = Object.freeze([
  "read", "write", "edit", "shell", "todo", "delete", "glob", "grep", "list",
]);

export function clientToolFamily(name) {
  const raw = String(name || "");
  const parts = raw.toLowerCase().split(/[^a-z0-9]+/).filter(Boolean);
  const candidates = new Set([normalizeIdentifier(raw)]);
  // Namespaced client tools often carry a transport prefix. Consider every
  // token-boundary suffix, while preserving compound spellings such as
  // write_file and run_terminal_cmd as one semantic candidate.
  for (let index = 1; index < parts.length; index++) {
    candidates.add(parts.slice(index).join(""));
  }
  const families = new Set();
  for (const candidate of candidates) {
    const index = TOOL_NAME_FAMILIES.findIndex((members) => members.includes(candidate));
    if (index >= 0) families.add(TOOL_FAMILY_KINDS[index]);
  }
  return families.size === 1 ? families.values().next().value : null;
}

function isObject(value) {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

function cloneJson(value) {
  if (Array.isArray(value)) return value.map(cloneJson);
  if (!isObject(value)) return value;
  return Object.fromEntries(Object.entries(value).map(([key, item]) => [key, cloneJson(item)]));
}

function localRefTarget(rootSchema, ref) {
  if (!isObject(rootSchema) || typeof ref !== "string" || !ref.startsWith("#/")) return null;
  let current = rootSchema;
  for (const rawPart of ref.slice(2).split("/")) {
    const part = rawPart.replace(/~1/g, "/").replace(/~0/g, "~");
    if (!isObject(current) && !Array.isArray(current)) return null;
    if (!Object.hasOwn(current, part)) return null;
    current = current[part];
  }
  return current;
}

// JSON Schema descriptors often factor tool inputs through local $defs refs.
// Ajv resolves those for validation; this materialized copy gives the
// normalization layer the same structural view without altering the exact
// schema advertised to Cursor or compiled for wire validation.
function materializeLocalRefs(schema, rootSchema = schema, activeRefs = new Set(), depth = 0) {
  if (depth > 64) return schema;
  if (Array.isArray(schema)) return schema.map((item) => materializeLocalRefs(item, rootSchema, activeRefs, depth + 1));
  if (!isObject(schema)) return schema;

  let source = schema;
  let childRefs = activeRefs;
  if (typeof schema.$ref === "string") {
    const target = localRefTarget(rootSchema, schema.$ref);
    if (target && !activeRefs.has(schema.$ref)) {
      const nextRefs = new Set(activeRefs);
      nextRefs.add(schema.$ref);
      childRefs = nextRefs;
      const resolved = materializeLocalRefs(target, rootSchema, nextRefs, depth + 1);
      if (isObject(resolved)) {
        source = { ...resolved, ...schema };
        delete source.$ref;
      }
    }
  }

  const out = { ...source };
  if (isObject(source.properties)) {
    out.properties = Object.fromEntries(Object.entries(source.properties)
      .map(([name, child]) => [name, materializeLocalRefs(child, rootSchema, childRefs, depth + 1)]));
  }
  for (const key of ["anyOf", "oneOf", "allOf", "prefixItems"]) {
    if (Array.isArray(source[key])) {
      out[key] = source[key].map((child) => materializeLocalRefs(child, rootSchema, childRefs, depth + 1));
    }
  }
  for (const key of ["items", "additionalProperties", "unevaluatedProperties", "contains", "not", "if", "then", "else", "propertyNames"]) {
    if (isObject(source[key]) || source[key] === true || source[key] === false) {
      out[key] = materializeLocalRefs(source[key], rootSchema, childRefs, depth + 1);
    }
  }
  return out;
}

function normalizeIdentifier(value) {
  return String(value || "").toLowerCase().replace(/[^a-z0-9]/g, "");
}

function jsonKind(value) {
  if (value === null) return "null";
  if (Array.isArray(value)) return "array";
  return typeof value === "object" ? "object" : typeof value;
}

function unsafeIntegerPath(value, path = "arguments", seen = new Set()) {
  if (typeof value === "number") {
    return Number.isInteger(value) && !Number.isSafeInteger(value) ? path : null;
  }
  if (!value || typeof value !== "object" || seen.has(value)) return null;
  seen.add(value);
  if (Array.isArray(value)) {
    for (let index = 0; index < value.length; index++) {
      const found = unsafeIntegerPath(value[index], `${path}.${index}`, seen);
      if (found) return found;
    }
    return null;
  }
  for (const key of Object.keys(value)) {
    const found = unsafeIntegerPath(value[key], `${path}.${key}`, seen);
    if (found) return found;
  }
  return null;
}

function stableJSON(value) {
  const normalize = (item) => {
    if (Array.isArray(item)) return item.map(normalize);
    if (!isObject(item)) return item;
    return Object.fromEntries(Object.keys(item).sort().map((key) => [key, normalize(item[key])]));
  };
  return JSON.stringify(normalize(value));
}

function schemaKinds(schema) {
  if (schema === true || !isObject(schema)) return null;
  if (schema === false) return new Set();
  const out = new Set();
  const declared = schema.type;
  if (typeof declared === "string") out.add(declared);
  else if (Array.isArray(declared)) {
    for (const item of declared) if (typeof item === "string") out.add(item);
  }
  for (const key of ["anyOf", "oneOf"]) {
    if (!Array.isArray(schema[key])) continue;
    for (const branch of schema[key]) {
      const branchKinds = schemaKinds(branch);
      if (branchKinds) for (const kind of branchKinds) out.add(kind);
    }
  }
  if (out.size === 0) {
    if (schema.properties || schema.required || schema.additionalProperties !== undefined) out.add("object");
    else if (schema.items || schema.prefixItems) out.add("array");
  }
  return out.size ? out : null;
}

function schemaAllowsKind(schema, kind) {
  const kinds = schemaKinds(schema);
  if (kinds === null) return true;
  if (kinds.has(kind)) return true;
  return kind === "number" && kinds.has("integer");
}

function schemaForValue(schema, value) {
  if (!isObject(schema)) return schema;
  for (const unionKey of ["anyOf", "oneOf"]) {
    const branches = schema[unionKey];
    if (!Array.isArray(branches)) continue;
    const matching = branches.filter((branch) => schemaAcceptsValue(branch, value));
    if (matching.length === 1) {
      // A union branch often carries only a discriminator or `required`
      // refinement. Replacing the parent with that branch discards the
      // parent's properties and prevents recursive field normalization. Keep
      // the parent's structural contract while applying the selected branch
      // as an additional refinement. Remove only the union being resolved so
      // later calls cannot select it repeatedly.
      const parent = { ...schema };
      delete parent[unionKey];
      return { allOf: [parent, matching[0]] };
    }
  }
  return schema;
}

function schemaAcceptsValue(schema, value) {
  if (schema === true) return true;
  if (schema === false) return false;
  if (!isObject(schema)) return true;
  let identity;
  try { identity = schemaCacheIdentity("normalization", schema); } catch { return schemaAllowsKind(schema, jsonKind(value)); }
  let validate = normalizationValidatorCache.get(identity.key);
  if (!validate) {
    try { validate = compileSchema("normalization branch", schema); }
    catch { return schemaAllowsKind(schema, jsonKind(value)); }
    normalizationValidatorCache.set(identity.key, validate, identity.bytes);
  }
  return validate(value) === true;
}

function parseContainerString(value) {
  if (typeof value !== "string") return undefined;
  const trimmed = value.trim();
  if (!trimmed || !/^(?:[\[{\"]|-?(?:0|[1-9]\d*)(?:\.\d+)?(?:[eE][+-]?\d+)?$|true$|false$|null$)/.test(trimmed)) return undefined;
  try {
    return JSON.parse(trimmed);
  } catch {
    return undefined;
  }
}

function decodeProtobufValue(value, depth = 0) {
  if (!isObject(value) || depth > 32) return { matched: false, value };
  const decodeChild = (child) => {
    const decoded = decodeProtobufValue(child, depth + 1);
    return decoded.matched ? decoded.value : child;
  };
  const direct = ["nullValue", "numberValue", "stringValue", "boolValue", "structValue", "listValue"]
    .filter((key) => Object.hasOwn(value, key));
  if (direct.length === 1) {
    const key = direct[0];
    if (key === "nullValue") return { matched: true, value: null };
    if (key === "numberValue" || key === "stringValue" || key === "boolValue") {
      return { matched: true, value: value[key] };
    }
    if (key === "listValue" && isObject(value.listValue) && Array.isArray(value.listValue.values)) {
      return { matched: true, value: value.listValue.values.map(decodeChild) };
    }
    if (key === "structValue" && isObject(value.structValue) && isObject(value.structValue.fields)) {
      return { matched: true, value: Object.fromEntries(Object.entries(value.structValue.fields)
        .map(([name, child]) => [name, decodeChild(child)])) };
    }
  }
  const kind = isObject(value.kind) ? value.kind : null;
  if (kind) {
    const discriminator = typeof kind.case === "string" ? kind.case
      : typeof kind.oneofKind === "string" ? kind.oneofKind : "";
    if (["nullValue", "numberValue", "stringValue", "boolValue", "structValue", "listValue"].includes(discriminator)) {
      const payload = Object.hasOwn(kind, "value") ? kind.value : kind[discriminator];
      return decodeProtobufValue({ [discriminator]: payload }, depth + 1);
    }
  }
  return { matched: false, value };
}

// Cursor's protobuf Value wrappers sometimes look like objects to JavaScript
// while JSON serialization reveals the actual primitive/container value.
function unwrapJsonLikeObject(value) {
  if (!isObject(value)) return undefined;
  try {
    const proto = Object.getPrototypeOf(value);
    if ((proto === Object.prototype || proto === null) && typeof value.toJSON !== "function") return undefined;
    const encoded = JSON.stringify(value);
    if (typeof encoded !== "string" || encoded.length === 0) return undefined;
    const parsed = JSON.parse(encoded);
    return isObject(parsed) && parsed === value ? undefined : parsed;
  } catch {
    return undefined;
  }
}

function protobufJsonValueType(value) {
  if (!isObject(value)) return "";
  const constructor = Object.getPrototypeOf(value)?.constructor;
  const typeName = constructor && typeof constructor.typeName === "string" ? constructor.typeName : "";
  if (!PROTOBUF_JSON_VALUE_TYPES.has(typeName)) return "";
  if (typeof value.toJson !== "function" || typeof value.toBinary !== "function") return "";
  return typeName;
}

function plainJsonContainer(value) {
  if (!isObject(value)) return false;
  const prototype = Object.getPrototypeOf(value);
  return prototype === Object.prototype || prototype === null;
}

// The patched Cursor seam receives MCP args before the SDK's native
// map<string, google.protobuf.Value> -> JSON conversion. Decode only
// positively identified protobuf JSON-value messages, then walk ordinary JSON
// containers to find nested messages. Arbitrary client objects and their
// toJson/toJSON methods are never invoked here.
function decodeProtocolValues(value, path, transforms, state = null, depth = 0) {
  const currentState = state || { nodes: 0, seen: new WeakSet() };
  if (depth > PROTOCOL_DECODE_MAX_DEPTH || currentState.nodes >= PROTOCOL_DECODE_MAX_NODES) return value;
  if (value === null || typeof value !== "object") return value;
  if (currentState.seen.has(value)) return value;
  currentState.seen.add(value);
  currentState.nodes++;

  const typeName = protobufJsonValueType(value);
  if (typeName) {
    try {
      const decoded = value.toJson();
      addTransform(transforms, {
        kind: "decode-protocol-value",
        path: path || "arguments",
        source: typeName,
        fromType: jsonKind(value),
        toType: jsonKind(decoded),
      });
      return decodeProtocolValues(decoded, path, transforms, currentState, depth + 1);
    } catch {
      return value;
    }
  }

  if (Array.isArray(value)) {
    let out = value;
    for (let index = 0; index < value.length; index++) {
      const childPath = path ? `${path}.${index}` : String(index);
      const child = decodeProtocolValues(value[index], childPath, transforms, currentState, depth + 1);
      if (child !== value[index]) {
        if (out === value) out = [...value];
        out[index] = child;
      }
    }
    return out;
  }
  if (!plainJsonContainer(value)) return value;

  let out = value;
  for (const key of Object.keys(value)) {
    const childPath = path ? `${path}.${key}` : key;
    const child = decodeProtocolValues(value[key], childPath, transforms, currentState, depth + 1);
    if (child !== value[key]) {
      if (out === value) out = { ...value };
      Object.defineProperty(out, key, { value: child, writable: true, enumerable: true, configurable: true });
    }
  }
  return out;
}

function wrapperCandidates(value, propertyName, expectedKinds) {
  const out = [];
  const add = (candidate, source) => {
    if (candidate === undefined || candidate === value) return;
    if (out.some((entry) => entry.value === candidate)) return;
    out.push({ value: candidate, source });
  };

  const serialized = unwrapJsonLikeObject(value);
  if (serialized !== undefined) add(serialized, "json-value");
  const protobuf = decodeProtobufValue(value);
  if (protobuf.matched) {
    add(protobuf.value, "protobuf-value");
    // A positively identified protobuf Value is itself the wrapper contract.
    // Do not also interpret its implementation keys as generic wrappers.
    return out;
  }
  const parsed = parseContainerString(value);
  if (parsed !== undefined) add(parsed, "json-string");
  if (!isObject(value)) return out;

  if (!expectedKinds || expectedKinds.has("array")) {
    const own = Object.keys(value);
    if (own.length && own.every((key, index) => String(index) === key)) {
      add(own.map((key) => value[key]), "indexed-object");
    }
  }

  const keys = [propertyName, ...GENERIC_WRAPPER_KEYS];
  if (!expectedKinds || expectedKinds.has("array")) keys.push(...ARRAY_WRAPPER_KEYS);
  if (!expectedKinds || expectedKinds.has("object")) keys.push(...OBJECT_WRAPPER_KEYS);
  if (!expectedKinds || expectedKinds.has("string")) keys.push(...STRING_WRAPPER_KEYS);
  if (expectedKinds && Object.keys(value).length === 1
    && [...expectedKinds].some((kind) => ["string", "number", "integer", "boolean", "null"].includes(kind))) {
    keys.push(...SCALAR_VARIANT_WRAPPER_KEYS);
  }
  for (const key of [...new Set(keys.filter(Boolean))]) {
    if (Object.hasOwn(value, key)) add(value[key], `property:${key}`);
  }
  const own = Object.keys(value);
  if (own.length === 1) add(value[own[0]], `sole-property:${own[0]}`);
  return out;
}

function coerceWrappedScalar(value, expectedKinds) {
  if (typeof value !== "string" || !expectedKinds) return undefined;
  const trimmed = value.trim();
  if ((expectedKinds.has("number") || expectedKinds.has("integer")) && trimmed !== "") {
    if (!/^-?(?:0|[1-9]\d*)(?:\.\d+)?(?:[eE][+-]?\d+)?$/.test(trimmed)) return undefined;
    const number = Number(trimmed);
    if (Number.isFinite(number)
      && (!expectedKinds.has("integer") || Number.isSafeInteger(number))
      && (!Number.isInteger(number) || Number.isSafeInteger(number))) return number;
  }
  if (expectedKinds.has("boolean")) {
    if (trimmed === "true") return true;
    if (trimmed === "false") return false;
  }
  return undefined;
}

function addTransform(transforms, transform) {
  if (transforms.length < 128) transforms.push(transform);
}

function unwrapToSchema(value, schema, propertyName, path, transforms, depth = 0) {
  const selectedSchema = schemaForValue(schema, value);
  const currentKind = jsonKind(value);

  // Never unwrap a value whose JSON kind is already legal. That is the guard
  // that prevents a legitimate nested object from being mistaken for an SDK
  // wrapper merely because it has a key named value/items/data.
  if (schemaAcceptsValue(selectedSchema, value)) return value;

  const expectedKinds = schemaKinds(selectedSchema);
  const accepted = new Map();
  for (const candidate of wrapperCandidates(value, propertyName, expectedKinds)) {
    let candidateValue = candidate.value;
    let candidateKind = jsonKind(candidateValue);
    const candidateTransforms = [];
    const coerced = coerceWrappedScalar(candidateValue, expectedKinds);
    if (coerced !== undefined) {
      candidateValue = coerced;
      candidateKind = jsonKind(candidateValue);
    }
    // A wrapper can contain another wrapper (for example, a todo list/items/task shape
    // and protobuf-style JSON values both do this). Normalize the candidate in
    // isolation before deciding whether it is the unique schema-valid unwrap.
    // Rejected candidates never leak transforms into the real receipt.
    if (!schemaAcceptsValue(selectedSchema, candidateValue) && depth < 32) {
      candidateValue = normalizeValue(
        candidateValue,
        selectedSchema,
        propertyName,
        path,
        candidateTransforms,
        depth + 1,
      );
      candidateKind = jsonKind(candidateValue);
    }
    if (!schemaAcceptsValue(selectedSchema, candidateValue)) continue;
    let candidateKey;
    try { candidateKey = `${candidateKind}:${JSON.stringify(candidateValue)}`; }
    catch { candidateKey = `${candidateKind}:${candidate.source}`; }
    if (!accepted.has(candidateKey)) {
      accepted.set(candidateKey, {
        ...candidate,
        value: candidateValue,
        kind: candidateKind,
        transforms: candidateTransforms,
      });
    }
  }
  if (accepted.size === 1) {
    const candidate = accepted.values().next().value;
    addTransform(transforms, {
      kind: "unwrap",
      path: path || "arguments",
      source: candidate.source,
      fromType: currentKind,
      toType: candidate.kind,
    });
    for (const transform of candidate.transforms) addTransform(transforms, transform);
    return candidate.value;
  }
  return value;
}

function schemaProperties(schema) {
  if (!isObject(schema)) return {};
  const entries = isObject(schema.properties) ? Object.entries(schema.properties) : [];
  if (Array.isArray(schema.allOf)) {
    for (const branch of schema.allOf) entries.push(...Object.entries(schemaProperties(branch)));
  }
  // Map/fromEntries preserves own JSON keys such as `__proto__`; assigning
  // those keys into `{}` invokes the legacy prototype setter and silently
  // removes them from the normalization view.
  return Object.fromEntries(new Map(entries));
}

function schemaRequired(schema) {
  if (!isObject(schema)) return new Set();
  const required = new Set(Array.isArray(schema.required) ? schema.required.filter((item) => typeof item === "string") : []);
  if (Array.isArray(schema.allOf)) {
    for (const branch of schema.allOf) for (const item of schemaRequired(branch)) required.add(item);
  }
  for (const unionKey of ["anyOf", "oneOf"]) {
    if (!Array.isArray(schema[unionKey]) || schema[unionKey].length === 0) continue;
    const branchSets = schema[unionKey].map(schemaRequired);
    for (const item of branchSets[0]) {
      if (branchSets.every((branch) => branch.has(item))) required.add(item);
    }
  }
  return required;
}

function omitOptionalPlaceholders(value, schema, path, transforms) {
  if (!isObject(value)) return value;
  const properties = schemaProperties(schema);
  const required = schemaRequired(schema);
  let out = value;
  for (const [key, item] of Object.entries(value)) {
    const undefinedValue = item === undefined;
    if (undefinedValue) {
      if (out === value) out = { ...value };
      delete out[key];
      addTransform(transforms, { kind: "omit-undefined", path: path ? `${path}.${key}` : key });
      continue;
    }
    if (required.has(key) || !Object.hasOwn(properties, key)) continue;
    const emptyObject = isObject(item) && Object.keys(item).length === 0;
    if (!emptyObject) continue;
    if (emptyObject && schemaAllowsKind(properties[key], "object")) continue;
    if (out === value) out = { ...value };
    delete out[key];
    addTransform(transforms, { kind: "omit-optional-placeholder", path: path ? `${path}.${key}` : key });
  }
  return out;
}

function chooseAliasTarget(sourceKey, properties) {
  const propertyNames = Object.keys(properties);
  const sourceNormalized = normalizeIdentifier(sourceKey);
  const normalizedMatches = propertyNames.filter((name) => normalizeIdentifier(name) === sourceNormalized);
  if (normalizedMatches.length === 1) return { target: normalizedMatches[0], priority: 100 };
  if (normalizedMatches.length > 1) return null;

  const rule = ARGUMENT_ALIASES[sourceNormalized];
  if (!rule) return null;
  const [candidates, priority] = rule;
  for (const candidate of candidates) {
    const matches = propertyNames.filter((name) => normalizeIdentifier(name) === normalizeIdentifier(candidate));
    if (matches.length === 1) return { target: matches[0], priority };
    if (matches.length > 1) return null;
  }
  return null;
}

function nestedArgumentObject(value) {
  if (isObject(value)) return value;
  if (typeof value !== "string" || !value.trim().startsWith("{")) return null;
  try {
    const parsed = JSON.parse(value);
    return isObject(parsed) ? parsed : null;
  } catch {
    return null;
  }
}

function flattenArgumentEnvelope(value, schema, path, transforms, depth = 0) {
  if (!isObject(value)) return value;
  if (depth > 16) return value;
  const properties = schemaProperties(schema);
  for (const envelopeKey of ENVELOPE_KEYS) {
    if (Object.hasOwn(properties, envelopeKey)) continue;
    let nested = nestedArgumentObject(value[envelopeKey]);
    if (!nested) continue;
    nested = flattenArgumentEnvelope(nested, schema, path, transforms, depth + 1);
    const nestedKeys = Object.keys(nested);
    if (!nestedKeys.length) continue;
    const recognized = nestedKeys.some((key) => Object.hasOwn(properties, key) || chooseAliasTarget(key, properties));
    if (!recognized) continue;
    for (const key of nestedKeys) {
      if (Object.hasOwn(value, key) && stableJSON(value[key]) !== stableJSON(nested[key])) {
        throw new ToolContractNormalizationError(
          "ambiguous_envelope",
          path ? `${path}.${key}` : key,
          `argument '${key}' conflicts between '${envelopeKey}' and the outer object`,
        );
      }
    }
    const out = { ...nested, ...value };
    delete out[envelopeKey];
    addTransform(transforms, { kind: "flatten-envelope", path: path || "arguments", source: envelopeKey });
    return out;
  }
  return value;
}

function normalizeObjectKeys(value, schema, path, transforms, depth = 0) {
  if (!isObject(value)) return value;
  const properties = schemaProperties(schema);
  if (!Object.keys(properties).length) return value;

  const candidates = new Map();
  for (const source of Object.keys(value).sort()) {
    const match = Object.hasOwn(properties, source)
      ? { target: source, priority: 120 }
      : chooseAliasTarget(source, properties);
    if (!match) continue;
    if (!candidates.has(match.target)) candidates.set(match.target, []);
    candidates.get(match.target).push({ source, priority: match.priority });
  }
  if (!candidates.size) return value;

  const assignments = new Map();
  for (const [target, sources] of candidates) {
    const childSchema = properties[target];
    const normalized = sources.map((entry) => {
      const candidateTransforms = [];
      const childPath = path ? `${path}.${target}` : target;
      const candidateValue = normalizeValue(value[entry.source], childSchema, target, childPath, candidateTransforms, depth + 1);
      return { ...entry, candidateTransforms, candidateValue };
    });
    const exact = normalized.find((entry) => entry.source === target);
    const selected = exact || normalized[0];
    const selectedJSON = stableJSON(selected.candidateValue);
    // Two source fields targeting one semantic argument are harmless only
    // when their normalized values are identical. An exact spelling does not
    // authorize silently discarding a conflicting alias.
    if (normalized.some((entry) => stableJSON(entry.candidateValue) !== selectedJSON)) {
      throw new ToolContractNormalizationError(
        "ambiguous_alias",
        path ? `${path}.${target}` : target,
        `multiple arguments map to '${target}' with different values`,
      );
    }
    normalized.sort((a, b) => b.priority - a.priority || a.source.localeCompare(b.source));
    const assignment = exact || normalized[0];
    assignments.set(target, assignment);
  }

  const mappedSources = new Set();
  for (const assignment of assignments.values()) mappedSources.add(assignment.source);
  const out = Object.create(null);
  for (const [source, item] of Object.entries(value)) {
    const mapped = Object.hasOwn(properties, source) || chooseAliasTarget(source, properties);
    if (!mapped) out[source] = item;
  }
  for (const [target, assignment] of assignments) {
    out[target] = assignment.candidateValue;
    for (const transform of assignment.candidateTransforms) addTransform(transforms, transform);
    if (assignment.source !== target) {
      addTransform(transforms, {
        kind: "rename-key",
        path: path || "arguments",
        source: assignment.source,
        target,
      });
    }
  }
  for (const source of Object.keys(value)) {
    if (mappedSources.has(source)) continue;
    const match = Object.hasOwn(properties, source) ? { target: source } : chooseAliasTarget(source, properties);
    if (match && assignments.has(match.target)) {
      addTransform(transforms, {
        kind: "deduplicate-alias",
        path: path || "arguments",
        source,
        target: match.target,
      });
    }
  }
  return Object.fromEntries(Object.entries(out));
}

function explicitAdvisoryIgnored(propertySchema) {
  return isObject(propertySchema) && propertySchema["x-cliproxy-advisory-ignored"] === true;
}

function stripAdvisoryFields(value, schema, path, transforms) {
  if (!isObject(value)) return value;
  const properties = schemaProperties(schema);
  let out = value;
  for (const [key, propertySchema] of Object.entries(properties)) {
    if (!explicitAdvisoryIgnored(propertySchema) || !Object.hasOwn(value, key)) continue;
    if (out === value) out = { ...value };
    delete out[key];
    addTransform(transforms, { kind: "strip-advisory", path: path ? `${path}.${key}` : key });
  }
  return out;
}

function replaceRootField(value, key, nextValue, transforms, transform) {
  if (stableJSON(value[key]) === stableJSON(nextValue)) return value;
  const out = Object.fromEntries(Object.entries(value));
  Object.defineProperty(out, key, { value: nextValue, writable: true, enumerable: true, configurable: true });
  addTransform(transforms, transform);
  return out;
}

function safeLineNumber(value) {
  return Number.isSafeInteger(value) && value >= 1;
}

// Structured read selectors are inclusive, 1-indexed strings. Preserve the exact
// range when a model emits the same value as a small structural record. No
// offset/limit guessing is allowed because offsets are commonly 0-indexed.
function structuredReadSelector(value) {
  if (Array.isArray(value) && value.length === 2
    && safeLineNumber(value[0]) && safeLineNumber(value[1]) && value[1] >= value[0]) {
    return `${value[0]}-${value[1]}`;
  }
  if (!isObject(value)) return undefined;
  const entries = Object.entries(value);
  const byName = new Map(entries.map(([key, item]) => [normalizeIdentifier(key), item]));
  const rangePairs = [
    ["start", "end"],
    ["startline", "endline"],
    ["linestart", "lineend"],
    ["from", "to"],
  ];
  for (const [startKey, endKey] of rangePairs) {
    if (entries.length !== 2 || !byName.has(startKey) || !byName.has(endKey)) continue;
    const start = byName.get(startKey);
    const end = byName.get(endKey);
    if (safeLineNumber(start) && safeLineNumber(end) && end >= start) return `${start}-${end}`;
  }
  const countPairs = [["start", "count"], ["startline", "count"]];
  for (const [startKey, countKey] of countPairs) {
    if (entries.length !== 2 || !byName.has(startKey) || !byName.has(countKey)) continue;
    const start = byName.get(startKey);
    const count = byName.get(countKey);
    if (safeLineNumber(start) && safeLineNumber(count)) return `${start}+${count}`;
  }
  return undefined;
}

function structuredReadSelectorCount(value) {
  if (typeof value !== "string") return undefined;
  let match = /^(\d+)-(\d+)$/.exec(value);
  if (match) {
    const start = Number(match[1]);
    const end = Number(match[2]);
    return Number.isSafeInteger(start) && Number.isSafeInteger(end) && end >= start
      ? end - start + 1 : undefined;
  }
  match = /^(\d+)\+(\d+)$/.exec(value);
  if (!match) return undefined;
  const count = Number(match[2]);
  return Number.isSafeInteger(count) && count >= 1 ? count : undefined;
}

function singletonJobIdList(value) {
  if (typeof value === "string" && value.length > 0) return [value];
  if (!isObject(value) || Object.keys(value).length !== 1) return undefined;
  const [key, item] = Object.entries(value)[0];
  if (!["id", "jobid", "poll"].includes(normalizeIdentifier(key))) return undefined;
  return typeof item === "string" && item.length > 0 ? [item] : undefined;
}

function normalizeProductionToolCompatibility(name, value, schema, transforms) {
  if (!isObject(value)) return value;
  const family = clientToolFamily(name);
  const properties = schemaProperties(schema);
  let out = value;

  if (family === "read" && Object.hasOwn(properties, "selector") && Object.hasOwn(out, "selector")) {
    const selector = structuredReadSelector(out.selector);
    if (selector !== undefined && schemaAcceptsValue(properties.selector, selector)) {
      out = replaceRootField(out, "selector", selector, transforms, {
        kind: "normalize-read-selector",
        path: "selector",
        fromType: jsonKind(value.selector),
        toType: "string",
      });
    }
    // An undeclared limit is redundant only when an exact, schema-valid
    // selector already preserves the requested read scope. With path+limit
    // alone, dropping limit would silently expand a bounded read to the whole
    // file, so leave it visible to fail-closed schema validation.
    if (schema.additionalProperties === false
      && Object.hasOwn(out, "limit") && !Object.hasOwn(properties, "limit")
      && schemaAcceptsValue(properties.selector, out.selector)
      && Number.isSafeInteger(out.limit) && out.limit >= 1
      && structuredReadSelectorCount(out.selector) === out.limit) {
      out = Object.fromEntries(Object.entries(out).filter(([key]) => key !== "limit"));
      addTransform(transforms, { kind: "strip-presentation", path: "limit", family });
    }
  }

  if (normalizeIdentifier(name) === "job" && Object.hasOwn(properties, "poll") && Object.hasOwn(out, "poll")
    && !schemaAcceptsValue(properties.poll, out.poll)) {
    const poll = singletonJobIdList(out.poll);
    if (poll !== undefined && schemaAcceptsValue(properties.poll, poll)) {
      out = replaceRootField(out, "poll", poll, transforms, {
        kind: "wrap-singleton-list",
        path: "poll",
        fromType: jsonKind(value.poll),
        toType: "array",
      });
    }
  }

  return out;
}

function dynamicPropertySchema(schema, key) {
  if (!isObject(schema)) return null;
  const matches = [];
  if (isObject(schema.patternProperties)) {
    for (const [pattern, childSchema] of Object.entries(schema.patternProperties)) {
      try { if (new RegExp(pattern).test(key)) matches.push(childSchema); } catch {}
    }
  }
  if (matches.length === 1) return matches[0];
  if (matches.length > 1) return { allOf: matches };
  return isObject(schema.additionalProperties) ? schema.additionalProperties : null;
}

function normalizeValue(value, schema, propertyName, path, transforms, depth = 0) {
  if (depth > 32) return value;
  let normalized = value;
  // Argument envelopes have stronger semantics than generic single-property
  // wrappers, so recognize them first and retain an accurate transform receipt.
  if (isObject(normalized)) {
    normalized = flattenArgumentEnvelope(normalized, schemaForValue(schema, normalized), path, transforms);
  }
  normalized = unwrapToSchema(normalized, schema, propertyName, path, transforms, depth);
  const selectedSchema = schemaForValue(schema, normalized);

  if (Array.isArray(normalized)) {
    const prefixItems = Array.isArray(selectedSchema && selectedSchema.prefixItems) ? selectedSchema.prefixItems : [];
    const itemSchema = selectedSchema && isObject(selectedSchema.items) ? selectedSchema.items : null;
    let out = normalized;
    for (let index = 0; index < normalized.length; index++) {
      const childSchema = prefixItems[index] || itemSchema;
      if (!childSchema) continue;
      const child = normalizeValue(normalized[index], childSchema, String(index), `${path}.${index}`, transforms, depth + 1);
      if (child !== normalized[index]) {
        if (out === normalized) out = [...normalized];
        out[index] = child;
      }
    }
    return out;
  }
  if (!isObject(normalized)) return normalized;

  normalized = flattenArgumentEnvelope(normalized, selectedSchema, path, transforms);
  normalized = normalizeObjectKeys(normalized, selectedSchema, path, transforms, depth);
  normalized = stripAdvisoryFields(normalized, selectedSchema, path, transforms);
  normalized = omitOptionalPlaceholders(normalized, selectedSchema, path, transforms);
  const properties = schemaProperties(selectedSchema);
  let out = normalized;
  for (const key of Object.keys(normalized)) {
    const childSchema = properties[key] || dynamicPropertySchema(selectedSchema, key);
    if (!childSchema) continue;
    const childPath = path ? `${path}.${key}` : key;
    const child = normalizeValue(normalized[key], childSchema, key, childPath, transforms, depth + 1);
    if (child !== normalized[key]) {
      if (out === normalized) out = { ...normalized };
      Object.defineProperty(out, key, { value: child, writable: true, enumerable: true, configurable: true });
    }
  }
  return out;
}

function normalizeRootInput(input, schema, transforms) {
  if (input === undefined) return {};
  if (input === null) return null;
  input = decodeProtocolValues(input, "", transforms);
  if (isObject(input)) {
    const serialized = unwrapJsonLikeObject(input);
    if (serialized !== undefined && jsonKind(serialized) !== "object") {
      addTransform(transforms, {
        kind: "unwrap-root",
        path: "arguments",
        source: "json-value",
        fromType: "object",
        toType: jsonKind(serialized),
      });
      input = serialized;
    }
  }
  if (isObject(input)) return normalizeValue(input, schema, "arguments", "", transforms);

  const properties = schemaProperties(schema);
  const propertyNames = Object.keys(properties);
  let target = Object.hasOwn(properties, "input") ? "input" : null;
  if (!target && propertyNames.length === 1) target = propertyNames[0];
  if (!target) target = "input";
  addTransform(transforms, {
    kind: "wrap-root",
    path: "arguments",
    target,
    fromType: jsonKind(input),
    toType: "object",
  });
  return normalizeValue({ [target]: input }, schema, "arguments", "", transforms);
}

function explicitDecoration(propertySchema) {
  if (!isObject(propertySchema)) return false;
  return propertySchema["x-cliproxy-client-decoration"] === true
    || propertySchema["x-client-decoration"] === true
    || propertySchema["x-harness-decoration"] === true;
}

function deriveAcceptanceSchema(schema) {
  if (Array.isArray(schema)) return schema.map((item) => deriveAcceptanceSchema(item));
  if (!isObject(schema)) return schema;
  const out = { ...schema };
  let changed = false;

  if (isObject(schema.properties)) {
    const properties = Object.create(null);
    const decorations = new Set();
    for (const [name, propertySchema] of Object.entries(schema.properties)) {
      const decoration = explicitDecoration(propertySchema)
        || explicitAdvisoryIgnored(propertySchema);
      properties[name] = decoration ? true : deriveAcceptanceSchema(propertySchema);
      if (decoration) decorations.add(name);
    }
    if (decorations.size && Array.isArray(schema.required)) {
      const required = schema.required.filter((name) => !decorations.has(name));
      if (required.length !== schema.required.length) {
        out.required = required;
        changed = true;
      }
    }
    if (Object.values(properties).some((value, index) => value !== Object.values(schema.properties)[index])) {
      out.properties = Object.fromEntries(Object.entries(properties));
      changed = true;
    }
  }
  for (const key of ["anyOf", "oneOf", "allOf", "prefixItems"]) {
    if (!Array.isArray(schema[key])) continue;
    const next = schema[key].map((item) => deriveAcceptanceSchema(item));
    if (next.some((value, index) => value !== schema[key][index])) {
      out[key] = next;
      changed = true;
    }
  }
  if (isObject(schema.items)) {
    const items = deriveAcceptanceSchema(schema.items);
    if (items !== schema.items) {
      out.items = items;
      changed = true;
    }
  }
  for (const key of ["$defs", "definitions", "dependentSchemas", "patternProperties"]) {
    if (!isObject(schema[key])) continue;
    const next = Object.fromEntries(Object.entries(schema[key])
      .map(([name, child]) => [name, deriveAcceptanceSchema(child)]));
    if (Object.keys(next).some((name) => next[name] !== schema[key][name])) {
      out[key] = next;
      changed = true;
    }
  }
  for (const key of ["additionalProperties", "unevaluatedProperties", "contains", "not", "if", "then", "else", "propertyNames"]) {
    if (!isObject(schema[key])) continue;
    const next = deriveAcceptanceSchema(schema[key]);
    if (next !== schema[key]) {
      out[key] = next;
      changed = true;
    }
  }
  return changed ? out : schema;
}

function ajvForSchema(schema) {
  const dialect = typeof schema.$schema === "string" ? schema.$schema : "";
  if (/draft-04/i.test(dialect)) return AjvDraft04;
  return /2020-12/i.test(dialect) ? Ajv2020 : /2019-09/i.test(dialect) ? Ajv2019 : Ajv;
}

function unresolvedSchemaReference(error) {
  const message = String(error && error.message || error);
  return /can't resolve reference|can't resolve schema|no schema with key or ref/i.test(message);
}

function ajvPrototypeSafeSchema(schema, seen = new WeakMap()) {
  if (!schema || typeof schema !== "object") return schema;
  if (seen.has(schema)) return seen.get(schema);
  if (Array.isArray(schema)) {
    const out = [];
    seen.set(schema, out);
    out.push(...schema.map((child) => ajvPrototypeSafeSchema(child, seen)));
    return out;
  }
  const out = Object.fromEntries(Object.entries(schema)
    .map(([key, child]) => [key, ajvPrototypeSafeSchema(child, seen)]));
  seen.set(schema, out);
  if (isObject(out.properties) && Object.hasOwn(out.properties, "__proto__")) {
    const exactPattern = "^__proto__$";
    const patterns = isObject(out.patternProperties) ? out.patternProperties : {};
    const declared = out.properties.__proto__;
    const exact = Object.hasOwn(patterns, exactPattern)
      ? { allOf: [patterns[exactPattern], declared] }
      : declared;
    // Ajv's additionalProperties fast path historically looks up property
    // names through ordinary object semantics. The exact pattern keeps this
    // legal JSON key both evaluated and type-checked without changing the
    // descriptor sent to Cursor or the harness.
    out.patternProperties = Object.fromEntries([
      ...Object.entries(patterns).filter(([key]) => key !== exactPattern),
      [exactPattern, exact],
    ]);
  }
  return out;
}

function deferredHarnessValidator(reason) {
  const validate = () => {
    validate.errors = null;
    return true;
  };
  validate.errors = null;
  validate.validationDeferredToHarness = String(reason || "unresolved schema reference");
  return validate;
}

function compileSchema(name, schema) {
  if (schema.$async === true) throw new Error("asynchronous JSON Schema validation is not supported for client tools");
  const unsafeSchemaNumber = unsafeIntegerPath(schema, "schema");
  if (unsafeSchemaNumber) {
    // JavaScript has already lost the original integer lexeme by this point.
    // Do not enforce a rounded bound in the bridge; the harness validates its
    // original schema. Executable arguments are independently refused below
    // if they contain an unsafe integer.
    return deferredHarnessValidator(`unsafe integer constraint at ${unsafeSchemaNumber}`);
  }
  const AjvForDialect = ajvForSchema(schema);
  try {
    return new AjvForDialect({
      allErrors: true,
      strict: false,
      allowUnionTypes: true,
      coerceTypes: false,
      useDefaults: false,
      removeAdditional: false,
      validateFormats: false,
      ownProperties: true,
    }).compile(ajvPrototypeSafeSchema(schema));
  } catch (error) {
    // Remote refs and uninstalled/unknown dialect metas are not proof that the
    // client's schema is invalid. Preserve the descriptor and let the harness
    // perform its authoritative validation instead of rejecting tool
    // registration with a false 422. Structurally malformed schemas still fail.
    if (unresolvedSchemaReference(error)) return deferredHarnessValidator(error && error.message);
    throw new Error(`advertised tool '${name}' has an invalid inputSchema: ${(error && error.message) || String(error)}`);
  }
}

function compiledContract(name, schema) {
  const identity = schemaCacheIdentity("strict", schema);
  const cached = validatorCache.get(identity.key);
  if (cached) return cached;
  const wireValidator = compileSchema(name, schema);
  const acceptanceSchema = deriveAcceptanceSchema(schema);
  const acceptanceValidator = acceptanceSchema === schema ? wireValidator : compileSchema(name, acceptanceSchema);
  const normalizationSchema = materializeLocalRefs(schema);
  const contract = Object.freeze({ schema, normalizationSchema, acceptanceSchema, wireValidator, acceptanceValidator });
  validatorCache.set(identity.key, contract, identity.bytes);
  return contract;
}

function validationErrorPath(error) {
  const parts = String(error && error.instancePath || "")
    .split("/")
    .filter(Boolean)
    .map((part) => part.replace(/~1/g, "/").replace(/~0/g, "~"));
  if (error && error.keyword === "required" && error.params && error.params.missingProperty) {
    parts.push(String(error.params.missingProperty));
  } else if (error && error.keyword === "additionalProperties" && error.params && error.params.additionalProperty) {
    parts.push(String(error.params.additionalProperty));
  }
  return parts.join(".") || "arguments";
}

function validationFailure(name, errors, transforms = []) {
  const rawErrors = Array.isArray(errors) ? errors : [];
  const normalized = rawErrors.slice(0, 64).map((error) => ({
    path: validationErrorPath(error),
    keyword: String(error && error.keyword || "schema"),
    message: String(error && error.message || "does not match the advertised schema"),
  }));
  const shown = normalized.slice(0, 12);
  const lines = shown.map((error) => `- ${error.path}: ${error.message}`);
  if (normalized.length > shown.length) lines.push(`- … ${normalized.length - shown.length} more schema errors`);
  return {
    content: `Client tool '${name}' was not executed because its arguments failed the advertised JSON schema:\n${lines.join("\n")}\nRetry the same tool with every required field and the exact declared types/enums.`,
    isError: true,
    structuredContent: {
      code: "client_tool_invalid_arguments",
      tool: name,
      errors: normalized,
      errorCount: rawErrors.length,
      errorsTruncated: rawErrors.length > normalized.length,
      executed: false,
      transforms,
    },
  };
}

export class ToolContractRegistry {
  constructor() {
    this.tools = Object.freeze([]);
    this.byName = new Map();
    this.contracts = new Map();
  }

  replace(tools) {
    const next = [];
    const byName = new Map();
    const contracts = new Map();
    for (const tool of Array.isArray(tools) ? tools : []) {
      const name = tool && (tool.toolName || tool.name);
      if (!name || typeof name !== "string") throw new Error("advertised tool is missing a string name");
      const inputSchema = tool.inputSchema === true || tool.inputSchema === false
        ? tool.inputSchema
        : (tool.inputSchema && typeof tool.inputSchema === "object" && !Array.isArray(tool.inputSchema)
          ? tool.inputSchema
          : { type: "object" });
      if (byName.has(name)) throw new Error(`advertised tool '${name}' appears more than once`);
      const normalized = Object.freeze({ ...tool, name, toolName: name, inputSchema });
      byName.set(name, normalized);
      next.push(normalized);
    }
    for (const tool of next) {
      contracts.set(tool.name, compiledContract(tool.name, tool.inputSchema));
    }
    this.byName = byName;
    this.contracts = contracts;
    this.tools = Object.freeze(next);
  }

  all() { return this.tools; }
  find(name) { return this.byName.get(name) || null; }

  normalize(name, input) {
    const contract = this.contracts.get(name);
    const transforms = [];
    const schema = contract ? contract.normalizationSchema : { type: "object" };
    let value = normalizeRootInput(input, schema, transforms);
    value = normalizeProductionToolCompatibility(name, value, schema, transforms);
    const unsafePath = unsafeIntegerPath(value);
    if (unsafePath) {
      throw new ToolContractNormalizationError(
        "unsafe_integer",
        unsafePath,
        "integer cannot be represented losslessly by the JavaScript SDK runtime; encode it as a string in the client contract",
      );
    }
    return { value, transforms };
  }

  validate(name, input, transforms = []) {
    const contract = this.contracts.get(name);
    if (!contract || contract.acceptanceValidator(input)) return null;
    return validationFailure(name, contract.acceptanceValidator.errors, transforms);
  }

  scoped(toolChoice, effectiveAdvertise) {
    return effectiveAdvertise(this.tools, toolChoice);
  }
}

export function normalizeToolArguments(input, schema = { type: "object" }) {
  const transforms = [];
  const value = normalizeRootInput(input, materializeLocalRefs(schema), transforms);
  const unsafePath = unsafeIntegerPath(value);
  if (unsafePath) {
    throw new ToolContractNormalizationError(
      "unsafe_integer",
      unsafePath,
      "integer cannot be represented losslessly by the JavaScript SDK runtime; encode it as a string in the client contract",
    );
  }
  return { value, transforms };
}

function resultImageFromBlock(block, path) {
  if (!block || block.type !== "image") return null;
  const source = isObject(block.source) ? block.source : isObject(block.image) ? block.image : block;
  if (source.kind === "url" || (typeof source.url === "string" && source.url)) {
    if (typeof source.url !== "string" || !source.url) {
      throw new ToolContractNormalizationError("invalid_result_image", path, "tool-result URL image is malformed");
    }
    return {
      url: source.url,
      ...(typeof source.mimeType === "string" && source.mimeType ? { mimeType: source.mimeType } : {}),
      ...(typeof source.detail === "string" && source.detail ? { detail: source.detail } : {}),
    };
  }
  const data = source.data;
  const mimeType = source.mimeType || source.media_type;
  if ((source.kind === "base64" || data !== undefined) && typeof data === "string" && data
    && typeof mimeType === "string" && mimeType) {
    return { data, mimeType };
  }
  throw new ToolContractNormalizationError("invalid_result_image", path, "tool-result image block is malformed");
}

export function normalizeToolResultEnvelope(
  content,
  isError,
  images,
  structuredContent,
  contentBlocks = undefined,
  structuredContentPresent = structuredContent !== undefined,
) {
  if (typeof isError !== "boolean") {
    throw new ToolContractNormalizationError("invalid_result_error_flag", "result.isError", "tool-result isError must be a boolean");
  }
  if (typeof structuredContentPresent !== "boolean") {
    throw new ToolContractNormalizationError(
      "invalid_structured_content_presence",
      "result.structuredContentPresent",
      "structuredContentPresent must be a boolean",
    );
  }
  const normalizedContent = content === undefined ? "" : content;
  const suppliedBlocks = contentBlocks !== undefined;
  let normalizedBlocks;
  if (suppliedBlocks) {
    if (!Array.isArray(contentBlocks)) {
      throw new ToolContractNormalizationError("invalid_result_blocks", "result.contentBlocks", "contentBlocks must be an array");
    }
    normalizedBlocks = cloneJson(contentBlocks);
  }
  const projectedImages = [];
  if (!suppliedBlocks && Array.isArray(images)) {
    for (let index = 0; index < images.length; index++) {
      const image = images[index];
      if (image && typeof image.url === "string" && image.url) {
        const normalized = {
          url: image.url,
          ...(typeof image.mimeType === "string" && image.mimeType ? { mimeType: image.mimeType } : {}),
          ...(typeof image.detail === "string" && image.detail ? { detail: image.detail } : {}),
        };
        projectedImages.push(normalized);
      } else if (image && typeof image.data === "string" && image.data && typeof image.mimeType === "string" && image.mimeType) {
        const normalized = { data: image.data, mimeType: image.mimeType };
        projectedImages.push(normalized);
      } else {
        throw new ToolContractNormalizationError("invalid_result_image", `result.images.${index}`, "tool-result image is malformed");
      }
    }
  }
  if (!suppliedBlocks) {
    normalizedBlocks = [
      ...(normalizedContent !== "" ? [{ type: "text", text: typeof normalizedContent === "string" ? normalizedContent : JSON.stringify(normalizedContent) }] : []),
      ...projectedImages.map((image) => ({
        type: "image",
        source: image.url
          ? { kind: "url", url: image.url, ...(image.mimeType ? { mimeType: image.mimeType } : {}), ...(image.detail ? { detail: image.detail } : {}) }
          : { kind: "base64", data: image.data, mimeType: image.mimeType },
      })),
    ];
  } else {
    for (let index = 0; index < normalizedBlocks.length; index++) {
      const image = resultImageFromBlock(normalizedBlocks[index], `result.contentBlocks.${index}`);
      if (image) projectedImages.push(image);
    }
  }
  const inlineImages = projectedImages.filter((image) => typeof image.data === "string");
  const urlImages = projectedImages.filter((image) => typeof image.url === "string");
  return {
    content: normalizedContent,
    contentBlocks: normalizedBlocks,
    isError,
    images: projectedImages.length ? projectedImages : null,
    inlineImages: inlineImages.length ? inlineImages : null,
    urlImages: urlImages.length ? urlImages : null,
    structuredContent: structuredContentPresent ? cloneJson(structuredContent) : null,
    structuredContentPresent,
  };
}

export function resolveClientToolName(want, tools, { onRejectedSingle } = {}) {
  const inventory = (Array.isArray(tools) ? tools : []).filter((tool) => tool && (tool.toolName || tool.name));
  const names = inventory.map((tool) => tool.toolName || tool.name);
  if (!names.length) return null;
  const normalizedWant = normalizeIdentifier(want);

  // A canonical advertised name always wins. An operator alias must never
  // steal an exact client capability and route it to a different tool.
  if (names.includes(want)) return want;
  const lower = String(want || "").toLowerCase();
  const caseInsensitive = names.filter((name) => name.toLowerCase() === lower);
  if (caseInsensitive.length === 1) return caseInsensitive[0];
  if (caseInsensitive.length > 1) return null;
  const normalizedMatches = names.filter((name) => normalizeIdentifier(name) === normalizedWant);
  if (normalizedMatches.length === 1) return normalizedMatches[0];
  if (normalizedMatches.length > 1) return null;

  // Operator-provided aliases are attached to exactly one advertised
  // descriptor by Go. Consult them only after canonical resolution, and
  // refuse aliases that collide with any canonical normalized spelling.
  const canonicalNormalized = new Set(names.map(normalizeIdentifier));
  if (!canonicalNormalized.has(normalizedWant)) {
    const explicitMatches = inventory.filter((tool) => Array.isArray(tool.aliases)
      && tool.aliases.some((alias) => normalizeIdentifier(alias) === normalizedWant));
    if (explicitMatches.length === 1) return explicitMatches[0].toolName || explicitMatches[0].name;
    if (explicitMatches.length > 1) return null;
  }

  // Native and client spellings form undirected semantic families: Cursor's
  // native `read` can target a harness `read_file`, and a model `ReadFile` can
  // target `_read`. A family is safe only when exactly one advertised
  // capability belongs to it.
  const family = TOOL_NAME_FAMILIES.find((members) => members.includes(normalizedWant));
  const aliasTargets = new Set((family || CROSS_FAMILY_TOOL_ALIASES[normalizedWant] || []).map(normalizeIdentifier));
  if (aliasTargets.size) {
    const matches = names.filter((name) => aliasTargets.has(normalizeIdentifier(name)));
    if (matches.length === 1) return matches[0];
    return null;
  }
  if (names.length === 1 && onRejectedSingle) onRejectedSingle(names[0]);
  return null;
}

export function acceptanceSchemaFor(schema) {
  return cloneJson(deriveAcceptanceSchema(schema));
}
