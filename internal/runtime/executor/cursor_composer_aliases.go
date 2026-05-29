package executor

import (
	"encoding/json"
	"regexp"
	"strings"
)

// This file mirrors composer-api/worker/openai.ts:1149-1455 — the post-
// processing layer that catches Composer-2.5 emitting subtly wrong tool
// names or argument keys.
//
// composer-api keeps these tables because Composer was trained on Cursor's
// IDE tool schemas (filePath, newString, oldString, ...) and often emits
// those names even when the client's schema uses different keys (path,
// new_content, search, ...). Without aliasing the client receives tool_use
// blocks it can't dispatch.

// normalizeToolName lowercases + strips non-alphanumerics. Matches
// `normalizeToolName` at openai.ts:1149-1151.
func normalizeToolName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// resolveToolSpec fuzzy-matches an emitted tool name against the client's
// inventory. Composer sometimes says "Read" when the client registered
// "read_file" or vice-versa; this returns the canonical client tool so the
// downstream OpenAI-format tool_calls[].function.name uses the name the
// client knows.
//
// Match order (mirrors composer-api openai.ts:1139-1147):
//  1. Exact name match
//  2. Normalized name match (case + punctuation stripped)
//  3. toolNameAliases table — Composer-native names → typical client names
//     (e.g. "read_file" → "Read"/"read", "run_terminal_cmd" → "bash"/"shell")
//
// Returns nil when no spec matches; callers should keep the raw name in
// that case (the client may still recognize it, or it's a hallucinated tool).
func resolveToolSpec(name string, tools []cursorToolDefinition, overrides map[string]string) *cursorToolDefinition {
	if name == "" || len(tools) == 0 {
		return nil
	}
	// 0. Caller-configured override (env CURSOR_TOOL_ALIASES / YAML tool-aliases) wins on conflict: map the
	//    emitted name (normalized) to a specific client tool. Falls through if the target tool isn't present.
	if len(overrides) > 0 {
		if target := overrides[normalizeToolName(name)]; target != "" {
			nt := normalizeToolName(target)
			for i := range tools {
				if tools[i].Name == target || normalizeToolName(tools[i].Name) == nt {
					return &tools[i]
				}
			}
		}
	}
	// 1. Exact match.
	for i := range tools {
		if tools[i].Name == name {
			return &tools[i]
		}
	}
	// 2. Normalized match.
	target := normalizeToolName(name)
	for i := range tools {
		if normalizeToolName(tools[i].Name) == target {
			return &tools[i]
		}
	}
	// 3. Alias table: emitted name → list of normalized client names.
	candidates := toolNameAliases(target)
	for _, cand := range candidates {
		for i := range tools {
			if normalizeToolName(tools[i].Name) == cand {
				return &tools[i]
			}
		}
	}
	return nil
}

// toolNameAliases maps a normalized Cursor-native tool name to a list of
// normalized client tool names that should match it. Mirrors composer-api
// openai.ts:1457-1477.
func toolNameAliases(normalized string) []string {
	aliases := map[string][]string{
		"createfile":     {"write"},
		"editfile":       {"edit"},
		"fileglob":       {"glob"},
		"filesearch":     {"glob", "grep"},
		"findfiles":      {"glob"},
		"openfile":       {"read"},
		"readfile":       {"read"},
		"replacefile":    {"edit"},
		"runterminalcmd": {"bash", "shell"},
		"shell":          {"bash"},
		"searchfiles":    {"grep", "glob"},
		"searchreplace":  {"edit"},
		"terminal":       {"bash", "shell"},
		"ls":             {"list"},
		"list":           {"ls"},
		"writefile":      {"write"},
	}
	return aliases[normalized]
}

// toolParameterSchema extracts the property names + required list from a
// parsed JSON-Schema parameters object. Mirrors openai.ts:1176-1189.
type toolParameterSchema struct {
	Properties []string
	Required   []string
}

func extractToolSchema(params map[string]any) toolParameterSchema {
	schema := toolParameterSchema{}
	if props, ok := params["properties"].(map[string]any); ok {
		for k := range props {
			schema.Properties = append(schema.Properties, k)
		}
	}
	if req, ok := params["required"].([]any); ok {
		for _, r := range req {
			if s, ok := r.(string); ok {
				schema.Required = append(schema.Required, s)
			}
		}
	}
	return schema
}

// argAliasRule is one entry in the alias table. Each rule maps a normalized
// emitted key to an ordered list of acceptable target keys plus a priority
// (higher wins when multiple emitted keys would map to the same target).
type argAliasRule struct {
	Candidates []string
	Priority   int
}

// commonArgumentAliases is the shared alias table (openai.ts:1348-1386).
// Keyed by the NORMALIZED form of the emitted argument name.
var commonArgumentAliases = map[string][]argAliasRule{
	"absolutepath":     {{Candidates: []string{"filePath", "path", "file", "filename"}, Priority: 80}},
	"commandline":      {{Candidates: []string{"command", "cmd", "script"}, Priority: 80}},
	"contents":         {{Candidates: []string{"content", "newString", "text"}, Priority: 70}},
	"cwd":              {{Candidates: []string{"cwd", "directory", "path", "pattern"}, Priority: 45}},
	"directory":        {{Candidates: []string{"directory", "cwd", "path", "pattern"}, Priority: 45}},
	"filetext":         {{Candidates: []string{"content", "text", "newString"}, Priority: 95}},
	"filepath":         {{Candidates: []string{"filePath", "path", "file", "filename"}, Priority: 90}},
	"filename":         {{Candidates: []string{"filePath", "path", "file", "filename"}, Priority: 75}},
	"glob":             {{Candidates: []string{"pattern", "glob", "include"}, Priority: 85}},
	"globpattern":      {{Candidates: []string{"pattern", "glob", "include"}, Priority: 95}},
	"include":          {{Candidates: []string{"include", "pattern", "glob"}, Priority: 70}},
	"newcontents":      {{Candidates: []string{"content", "newString", "replacement", "text"}, Priority: 85}},
	"newstring":        {{Candidates: []string{"newString", "replacement", "content"}, Priority: 95}},
	"newtext":          {{Candidates: []string{"newString", "replacement", "content", "text"}, Priority: 85}},
	"oldcontents":      {{Candidates: []string{"oldString", "old", "search", "text"}, Priority: 80}},
	"oldstring":        {{Candidates: []string{"oldString", "old", "search"}, Priority: 95}},
	"oldtext":          {{Candidates: []string{"oldString", "old", "search", "text"}, Priority: 85}},
	"pattern":          {{Candidates: []string{"pattern", "query", "regex", "search"}, Priority: 80}},
	"query":            {{Candidates: []string{"query", "pattern", "search", "prompt"}, Priority: 80}},
	"regex":            {{Candidates: []string{"pattern", "regex", "query"}, Priority: 75}},
	"replacement":      {{Candidates: []string{"newString", "replacement", "content"}, Priority: 85}},
	"script":           {{Candidates: []string{"command", "script", "cmd"}, Priority: 75}},
	"search":           {{Candidates: []string{"pattern", "query", "oldString", "search"}, Priority: 70}},
	"searchstring":     {{Candidates: []string{"pattern", "query", "oldString", "search"}, Priority: 80}},
	"targetdirectory":  {{Candidates: []string{"directory", "cwd", "path", "pattern"}, Priority: 55}},
	"targetfile":       {{Candidates: []string{"filePath", "path", "file", "filename"}, Priority: 90}},
	"targeting":        {{Candidates: []string{"path", "directory", "cwd", "pattern", "filePath"}, Priority: 45}},
	"url":              {{Candidates: []string{"url", "uri", "href"}, Priority: 90}},
	"workingdirectory": {{Candidates: []string{"workdir", "cwd", "directory", "path"}, Priority: 90}},
	"cmd":              {{Candidates: []string{"command", "cmd", "script"}, Priority: 95}},
	"path":             {{Candidates: []string{"filePath", "path", "directory", "cwd", "pattern"}, Priority: 75}},
	"prompt":           {{Candidates: []string{"prompt", "description", "instructions", "query"}, Priority: 80}},
	"tasks":            {{Candidates: []string{"todos", "tasks", "items"}, Priority: 75}},
	"todo":             {{Candidates: []string{"todos", "items", "tasks"}, Priority: 70}},
	"items":            {{Candidates: []string{"todos", "items", "tasks"}, Priority: 70}},
}

// toolSpecificArgumentAliases narrows the alias rules per tool. Mirrors
// openai.ts:1388-1455 (the per-tool branching block).
func toolSpecificArgumentAliases(toolNorm, keyNorm string) []argAliasRule {
	switch {
	case in([]string{"glob", "fileglob", "filesearch", "findfiles"}, toolNorm):
		if in([]string{"globpattern", "glob", "include", "pattern"}, keyNorm) {
			return []argAliasRule{{Candidates: []string{"pattern", "glob", "include"}, Priority: 98}}
		}
		if in([]string{"targeting", "targetdirectory", "cwd", "directory", "path"}, keyNorm) {
			return []argAliasRule{{Candidates: []string{"pattern", "path", "directory", "cwd"}, Priority: 40}}
		}
	case in([]string{"grep", "search", "searchfiles"}, toolNorm):
		if in([]string{"query", "search", "searchstring", "regex", "pattern"}, keyNorm) {
			return []argAliasRule{{Candidates: []string{"pattern", "query", "regex", "search"}, Priority: 95}}
		}
		if in([]string{"globpattern", "glob", "include"}, keyNorm) {
			return []argAliasRule{{Candidates: []string{"include", "glob", "files", "pattern"}, Priority: 75}}
		}
	case in([]string{"read", "readfile", "openfile"}, toolNorm):
		if in([]string{"targeting", "targetfile", "filepath", "absolutepath", "path", "file"}, keyNorm) {
			return []argAliasRule{{Candidates: []string{"filePath", "path", "file", "filename"}, Priority: 95}}
		}
	case in([]string{"write", "writefile", "createfile"}, toolNorm):
		if in([]string{"targeting", "targetfile", "filepath", "absolutepath", "path", "file"}, keyNorm) {
			return []argAliasRule{{Candidates: []string{"filePath", "path", "file", "filename"}, Priority: 95}}
		}
		if in([]string{"newcontents", "contents", "content", "text"}, keyNorm) {
			return []argAliasRule{{Candidates: []string{"content", "text", "newString"}, Priority: 95}}
		}
	}
	return nil
}

func in(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// normalizeToolArguments remaps Composer-emitted argument keys onto the
// client's actual schema using exact + normalized + alias-table matching.
// When no mapping is found and the schema doesn't allow additional
// properties, the offending key is dropped. Priorities resolve collisions
// (e.g. emitted "filePath" beats emitted "path" both targeting the same
// schema slot).
//
// Mirrors openai.ts:1153-1173. Schema-less tools (no parameters object)
// get their arguments passed through verbatim.
func normalizeToolArguments(args map[string]any, tool *cursorToolDefinition) map[string]any {
	if tool == nil || tool.Parameters == "" {
		return args
	}
	// Parse the schema FIRST so wrapper-key expansion can consult the set of
	// declared property names. A wrapper key (e.g. "input"/"params") that is
	// itself a REAL declared property must be preserved verbatim — only true
	// single-wrapper envelopes (keys absent from the schema) are unwrapped.
	var params map[string]any
	if err := jsonUnmarshalString(tool.Parameters, &params); err != nil {
		// Unparseable schema: fall back to the old unconditional expansion so
		// genuinely nested envelopes still flatten.
		return expandToolArguments(args, nil)
	}
	schema := extractToolSchema(params)
	if len(schema.Properties) == 0 {
		return expandToolArguments(args, nil)
	}
	toolNorm := normalizeToolName(tool.Name)
	normalizedProps := make(map[string]string, len(schema.Properties))
	declared := make(map[string]bool, len(schema.Properties))
	for _, p := range schema.Properties {
		norm := normalizeToolName(p)
		normalizedProps[norm] = p
		declared[norm] = true
	}

	// Step 1: expand nested wrapper keys ("arguments"/"args"/"input"/"parameters"/
	// "params"/"targeting" carrying a nested object), skipping any wrapper key
	// that is a declared property. Mirrors openai.ts:1280-1296.
	expanded := expandToolArguments(args, declared)

	allowAdditional := false
	if v, ok := params["additionalProperties"]; ok {
		if b, ok := v.(bool); ok && b {
			allowAdditional = true
		} else if _, ok := v.(map[string]any); ok {
			allowAdditional = true
		}
	}

	output := map[string]any{}
	priorities := map[string]int{}
	for key, value := range expanded {
		target, priority := mapArgKey(key, schema.Properties, normalizedProps, toolNorm)
		if target == "" {
			if allowAdditional {
				output[key] = value
			}
			continue
		}
		// Priority resolution: composer-api uses `>=` so equal priority
		// overwrites the previous value (deterministic on iteration order
		// matching JS object enumeration). We preserve the same semantic.
		if prev, ok := priorities[target]; !ok || priority >= prev {
			output[target] = value
			priorities[target] = priority
		}
	}
	// Step 2: fill in required arguments that the model omitted using
	// per-tool defaults (e.g. derive `description` from `command` for shell
	// tools, default `pattern` to "*" for glob tools). Mirrors openai.ts:1191-1213.
	output = applyRequiredToolDefaults(output, schema.Required, tool, expanded)
	return output
}

// expandToolArguments flattens common "nested args" patterns where the model
// emits a single key carrying the whole argument object (e.g. `{"arguments":
// {"path":"x"}}` instead of `{"path":"x"}`). Mirrors openai.ts:1280-1296.
//
// declared is the set of normalized property names the tool actually declares.
// A wrapper key that is itself a declared property (e.g. an MCP tool with a
// real `input`/`params`/`targeting` field) is preserved verbatim rather than
// unwrapped — only true single-wrapper envelopes (keys NOT in the schema) are
// flattened. A nil/empty declared set restores the old unconditional behavior.
func expandToolArguments(args map[string]any, declared map[string]bool) map[string]any {
	out := map[string]any{}
	for key, value := range args {
		norm := normalizeToolName(key)
		// A declared property named like a wrapper key is a real argument, not
		// an envelope: keep it untouched.
		if declared[norm] {
			out[key] = value
			continue
		}
		nested := recordArgumentValue(value)
		if nested != nil && in([]string{"arguments", "args", "input", "parameters", "params"}, norm) {
			for k, v := range expandToolArguments(nested, declared) {
				out[k] = v
			}
			continue
		}
		if nested != nil && norm == "targeting" {
			for k, v := range expandToolArguments(nested, declared) {
				out[k] = v
			}
			continue
		}
		out[key] = value
	}
	return out
}

// recordArgumentValue treats `value` as a nested object: accepts a real map
// or a JSON-encoded string that parses to an object. Mirrors openai.ts:1298-1307.
func recordArgumentValue(value any) map[string]any {
	if m, ok := value.(map[string]any); ok {
		return m
	}
	s, ok := value.(string)
	if !ok {
		return nil
	}
	trimmed := strings.TrimSpace(s)
	if !strings.HasPrefix(trimmed, "{") {
		return nil
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
		return nil
	}
	return parsed
}

// applyRequiredToolDefaults fills in arguments that the tool's schema marks
// as required but the model didn't emit. Per-tool heuristics:
//   - shell/bash/terminal: derive `description` from `command`; backfill
//     `command` from cmd/script aliases.
//   - glob family: default `pattern` to "*" or the closest alias key.
//
// Mirrors openai.ts:1191-1213.
func applyRequiredToolDefaults(output map[string]any, required []string, tool *cursorToolDefinition, originalArgs map[string]any) map[string]any {
	if len(required) == 0 || tool == nil {
		return output
	}
	toolNorm := normalizeToolName(tool.Name)
	next := make(map[string]any, len(output))
	for k, v := range output {
		next[k] = v
	}
	switch {
	case in([]string{"bash", "shell", "terminal"}, toolNorm):
		if requiredHas(required, "description") {
			if _, ok := next["description"].(string); !ok {
				next["description"] = shellDescription(next["command"])
			}
		}
		if requiredHas(required, "command") {
			if _, ok := next["command"].(string); !ok {
				if cmd := firstStringFromArgs(originalArgs, "command", "cmd", "script"); cmd != "" {
					next["command"] = cmd
				} else {
					next["command"] = ""
				}
			}
		}
	case in([]string{"glob", "fileglob", "filesearch", "findfiles"}, toolNorm):
		if requiredHas(required, "pattern") {
			if _, ok := next["pattern"].(string); !ok {
				if p := firstStringFromArgs(originalArgs, "globPattern", "glob", "include", "pattern"); p != "" {
					next["pattern"] = p
				} else {
					next["pattern"] = "*"
				}
			}
		}
	}
	return next
}

func requiredHas(required []string, key string) bool {
	for _, r := range required {
		if r == key {
			return true
		}
	}
	return false
}

// shellDescription derives a one-line description from the first few tokens
// of a shell command. Mirrors openai.ts:1240-1244.
func shellDescription(command any) string {
	s, _ := command.(string)
	if strings.TrimSpace(s) == "" {
		return "Runs shell command"
	}
	fields := strings.Fields(strings.TrimSpace(s))
	if len(fields) > 5 {
		fields = fields[:5]
	}
	return "Runs " + strings.Join(fields, " ")
}

// mapArgKey resolves a single emitted argument key. Returns the target
// schema property name and the priority of the match, or "" if unmappable.
func mapArgKey(key string, properties []string, normalizedProps map[string]string, toolNorm string) (string, int) {
	// Exact-name match wins.
	for _, p := range properties {
		if p == key {
			return p, 100
		}
	}
	// Normalized-name match.
	if target, ok := normalizedProps[normalizeToolName(key)]; ok {
		return target, 100
	}
	// Alias table (tool-specific first, then common).
	keyNorm := normalizeToolName(key)
	rules := append(toolSpecificArgumentAliases(toolNorm, keyNorm), commonArgumentAliases[keyNorm]...)
	for _, rule := range rules {
		if target := firstMatchingProperty(rule.Candidates, properties, normalizedProps); target != "" {
			return target, rule.Priority
		}
	}
	return "", 0
}

func firstMatchingProperty(candidates, properties []string, normalizedProps map[string]string) string {
	for _, c := range candidates {
		for _, p := range properties {
			if p == c {
				return p
			}
		}
		if target, ok := normalizedProps[normalizeToolName(c)]; ok {
			return target
		}
	}
	return ""
}

func jsonUnmarshalString(s string, v any) error {
	return json.Unmarshal([]byte(s), v)
}

// --- Workspace mutation heuristic (openai.ts:768-806) ---

var workspaceMutationIntentRegex = regexp.MustCompile(`(?i)\b(make|create|build|add|write|generate|scaffold|implement|set up|setup)\b`)

// hasCursorWorkspaceMutationIntent reports whether any user-role message
// asks for file edits. Used to gate the WORKSPACE MUTATION REQUIRED block.
// Mirrors openai.ts:768-774.
func hasCursorWorkspaceMutationIntent(messages []any) bool {
	var sb strings.Builder
	for _, m := range messages {
		mm, _ := m.(map[string]any)
		if mm == nil {
			continue
		}
		if role, _ := mm["role"].(string); role != "user" {
			continue
		}
		sb.WriteString(extractMessageContent(mm["content"]))
		sb.WriteByte('\n')
	}
	return workspaceMutationIntentRegex.MatchString(sb.String())
}

// hasCursorWorkspaceMutationToolCall reports whether the prior assistant
// turns already invoked a file-mutating tool. Mirrors openai.ts:776-806.
func hasCursorWorkspaceMutationToolCall(messages []any) bool {
	for _, m := range messages {
		mm, _ := m.(map[string]any)
		if mm == nil {
			continue
		}
		// role=tool with a name field (the server-side execution log).
		if name, _ := mm["name"].(string); name != "" && isCursorMutatingToolCall(name, nil) {
			return true
		}
		toolCalls, _ := mm["tool_calls"].([]any)
		for _, tc := range toolCalls {
			tcm, _ := tc.(map[string]any)
			if tcm == nil {
				continue
			}
			fn, _ := tcm["function"].(map[string]any)
			name, _ := fn["name"].(string)
			if isCursorMutatingToolCall(name, fn["arguments"]) {
				return true
			}
		}
	}
	return false
}

var fileMutatingRegexes = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(^|[\s;&|])(cat|printf|echo)\b[\s\S]*(>|>>|<<)`),
	regexp.MustCompile(`(?i)(^|[\s;&|])(tee|touch|cp|mv|rm)\b`),
	regexp.MustCompile(`(?i)(^|[\s;&|])sed\b[^\n]*(\s-i\b|\s-i['"]?\s)`),
	regexp.MustCompile(`(?i)(^|[\s;&|])perl\b[^\n]*(\s-pi\b|\s-pi['"]?\s)`),
	regexp.MustCompile(`(?i)(^|[\s;&|])(npm|pnpm|yarn|bun)\s+(init|install|add|create)\b`),
	regexp.MustCompile(`(?i)(>|>>)\s*(\.{0,2}/)?[a-z0-9._/-]+`),
}

func isCursorMutatingToolCall(name string, args any) bool {
	norm := normalizeToolName(name)
	if in([]string{"write", "writefile", "edit", "editfile"}, norm) {
		return true
	}
	if !in([]string{"bash", "shell", "terminal"}, norm) {
		return false
	}
	// Inspect command-style arguments for file-mutating shell syntax.
	cmd := firstStringFromArgs(args, "command", "cmd", "script")
	if cmd == "" {
		return false
	}
	for _, re := range fileMutatingRegexes {
		if re.MatchString(cmd) {
			return true
		}
	}
	return false
}

func firstStringFromArgs(args any, keys ...string) string {
	var parsed map[string]any
	switch a := args.(type) {
	case string:
		_ = json.Unmarshal([]byte(a), &parsed)
	case map[string]any:
		parsed = a
	}
	if parsed == nil {
		return ""
	}
	for _, k := range keys {
		if s, ok := parsed[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}
