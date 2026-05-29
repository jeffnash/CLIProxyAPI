package executor

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// AL1 regression: a tool that DECLARES a real property named like a wrapper key
// ("input"/"params"/"arguments"/...) must have that key preserved, not flattened
// away by the single-wrapper-envelope unwrapper.
func TestNormalizeToolArguments_DeclaredWrapperKeyPreserved(t *testing.T) {
	cases := []struct {
		name    string
		prop    string // the declared property that collides with a wrapper key
		wrapper string // the key the model emits (same as prop)
	}{
		{"input", "input", "input"},
		{"params", "params", "params"},
		{"arguments", "arguments", "arguments"},
		{"args", "args", "args"},
		{"parameters", "parameters", "parameters"},
		{"targeting", "targeting", "targeting"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tool := &cursorToolDefinition{
				Name:       "mcp_tool",
				Parameters: `{"type":"object","properties":{"` + c.prop + `":{"type":"object"}}}`,
			}
			// The MCP arg is a real nested object delivered under the declared
			// property; it must arrive intact, not be hoisted into the top level.
			payload := map[string]any{"foo": "bar", "n": float64(1)}
			raw := map[string]any{c.wrapper: payload}
			got := normalizeToolArguments(raw, tool)

			nested, ok := got[c.prop].(map[string]any)
			if !ok {
				t.Fatalf("declared property %q should be preserved as a nested object, got %+v", c.prop, got)
			}
			if nested["foo"] != "bar" || nested["n"] != float64(1) {
				t.Fatalf("nested object content lost: %+v", nested)
			}
			// The inner keys must NOT have been hoisted to the top level.
			if _, leaked := got["foo"]; leaked {
				t.Fatalf("wrapper envelope was wrongly flattened (foo leaked to top level): %+v", got)
			}
		})
	}
}

// AL1 regression: when the wrapper key is NOT a declared property, a true
// single-wrapper envelope still gets flattened (back-compat with the original
// behavior).
func TestNormalizeToolArguments_UndeclaredEnvelopeStillFlattens(t *testing.T) {
	// Tool declares file_path, NOT "arguments".
	tool := &cursorToolDefinition{
		Name:       "Read",
		Parameters: `{"type":"object","properties":{"file_path":{"type":"string"}}}`,
	}
	raw := map[string]any{"arguments": map[string]any{"file_path": "x.txt"}}
	got := normalizeToolArguments(raw, tool)
	if got["file_path"] != "x.txt" {
		t.Fatalf("true envelope should still flatten to file_path=x.txt, got %+v", got)
	}
	if _, present := got["arguments"]; present {
		t.Fatalf("undeclared wrapper key should not survive, got %+v", got)
	}
}

// AL1 regression: a nested envelope INSIDE a real (undeclared) envelope still
// expands — the recursion is preserved through the declared-set parameter.
func TestNormalizeToolArguments_NestedEnvelopeRecursionPreserved(t *testing.T) {
	tool := &cursorToolDefinition{
		Name:       "Read",
		Parameters: `{"type":"object","properties":{"file_path":{"type":"string"}}}`,
	}
	// arguments -> input -> file_path; neither "arguments" nor "input" is declared.
	raw := map[string]any{"arguments": map[string]any{"input": map[string]any{"file_path": "y.txt"}}}
	got := normalizeToolArguments(raw, tool)
	if got["file_path"] != "y.txt" {
		t.Fatalf("nested envelope should recurse and flatten to file_path=y.txt, got %+v", got)
	}
}

// expandToolArguments with a nil declared set keeps the original unconditional
// behavior (used for schema-less / unparseable-schema fallbacks).
func TestExpandToolArguments_NilDeclaredFlattens(t *testing.T) {
	raw := map[string]any{"params": map[string]any{"path": "z"}}
	out := expandToolArguments(raw, nil)
	if out["path"] != "z" {
		t.Fatalf("nil declared set should flatten the envelope, got %+v", out)
	}
	if _, present := out["params"]; present {
		t.Fatalf("envelope key should be gone with nil declared set, got %+v", out)
	}
}

// expandToolArguments preserves a declared wrapper key directly.
func TestExpandToolArguments_DeclaredKeyKept(t *testing.T) {
	declared := map[string]bool{"input": true}
	raw := map[string]any{"input": map[string]any{"a": "b"}}
	out := expandToolArguments(raw, declared)
	nested, ok := out["input"].(map[string]any)
	if !ok || nested["a"] != "b" {
		t.Fatalf("declared 'input' should be kept verbatim, got %+v", out)
	}
}

// --- ADD-44: deterministic argument-key collision resolution ---

// ADD-44: when the model emits BOTH an exact schema key and a normalized alias
// of it ({"filePath":"safe","file_path":"danger"} against a schema declaring
// `filePath`), the exact key MUST win every time — never a random flip driven
// by Go map iteration order. Run many times to defeat map randomization.
func TestNormalizeToolArguments_ExactBeatsNormalizedDeterministic(t *testing.T) {
	tool := &cursorToolDefinition{
		Name:       "Read",
		Parameters: `{"type":"object","properties":{"filePath":{"type":"string"}}}`,
	}
	for i := 0; i < 200; i++ {
		raw := map[string]any{"filePath": "safe.txt", "file_path": "danger.txt"}
		got := normalizeToolArguments(raw, tool)
		if got["filePath"] != "safe.txt" {
			t.Fatalf("iter %d: exact key must win deterministically; got filePath=%v (full=%+v)", i, got["filePath"], got)
		}
		if _, leaked := got["file_path"]; leaked {
			t.Fatalf("iter %d: normalized alias key should not survive alongside exact target; got %+v", i, got)
		}
	}
}

// ADD-44: a key supplied directly at the top level wins over the same key
// produced by unwrapping a nested envelope
// ({"path":"safe","arguments":{"path":"danger"}} → path=safe), deterministically.
func TestNormalizeToolArguments_TopLevelBeatsEnvelopeDeterministic(t *testing.T) {
	tool := &cursorToolDefinition{
		Name:       "Read",
		Parameters: `{"type":"object","properties":{"path":{"type":"string"}}}`,
	}
	for i := 0; i < 200; i++ {
		raw := map[string]any{
			"path":      "safe.txt",
			"arguments": map[string]any{"path": "danger.txt"},
		}
		got := normalizeToolArguments(raw, tool)
		if got["path"] != "safe.txt" {
			t.Fatalf("iter %d: top-level key must beat envelope-derived key; got path=%v (full=%+v)", i, got["path"], got)
		}
	}
}

// ADD-44: even when neither colliding key is an exact match (both reach the same
// schema slot via normalization at EQUAL priority), the winner must be STABLE
// across runs (first-writer by sorted key order), not random. We only assert
// stability + that the value is one of the two inputs — the precise winner is an
// implementation detail, but it must never differ run-to-run.
func TestNormalizeToolArguments_EqualPriorityCollisionStable(t *testing.T) {
	// Schema declares snake_case `file_path`; the model emits two distinct keys
	// that both normalize to `filepath` but neither equals `file_path` exactly.
	tool := &cursorToolDefinition{
		Name:       "Read",
		Parameters: `{"type":"object","properties":{"file_path":{"type":"string"}}}`,
	}
	var first string
	for i := 0; i < 200; i++ {
		raw := map[string]any{"filePath": "A", "FILE_PATH": "B"}
		got := normalizeToolArguments(raw, tool)
		v, _ := got["file_path"].(string)
		if v != "A" && v != "B" {
			t.Fatalf("iter %d: expected file_path to be one of the emitted values, got %v (full=%+v)", i, v, got)
		}
		if i == 0 {
			first = v
		} else if v != first {
			t.Fatalf("iter %d: equal-priority collision winner flipped (%q vs %q) — nondeterministic", i, v, first)
		}
	}
}

// expandToolArguments: top-level wins over envelope at the raw-expansion layer,
// independent of map iteration order.
func TestExpandToolArguments_TopLevelBeatsEnvelope(t *testing.T) {
	for i := 0; i < 200; i++ {
		raw := map[string]any{
			"path":      "top",
			"arguments": map[string]any{"path": "nested"},
		}
		out := expandToolArguments(raw, nil)
		if out["path"] != "top" {
			t.Fatalf("iter %d: top-level key must win over envelope, got %+v", i, out)
		}
	}
}

// --- ADD-51: fuzzy tool-name reconciliation disambiguates or refuses ---

// ADD-51: two client tools that NORMALIZE to the same value make a fuzzy
// (non-exact) match ambiguous. resolveToolSpec must refuse (return nil) so the
// caller preserves the raw emitted name, rather than silently routing to
// whichever tool is first in the inventory.
func TestResolveToolSpec_NormalizedCollisionRefuses(t *testing.T) {
	tools := []cursorToolDefinition{
		{Name: "mcp__server-a__run"},
		{Name: "mcp_server_a_run"},
	}
	// "mcpserverarun" is the shared normalized form; the emitted name matches
	// neither exactly, so this is a fuzzy match against two distinct tools.
	if spec := resolveToolSpec("mcp.server.a.run", tools, nil); spec != nil {
		t.Fatalf("normalized collision must refuse (nil), routed to %q instead", spec.Name)
	}
	// An EXACT emitted name still resolves unambiguously even though a normalized
	// collision exists in the inventory.
	if spec := resolveToolSpec("mcp__server-a__run", tools, nil); spec == nil || spec.Name != "mcp__server-a__run" {
		t.Fatalf("exact match must still resolve; got %+v", spec)
	}
}

// ADD-51: a SINGLE alias candidate that matches MULTIPLE distinct client tools
// (a normalized collision reached via the alias table, e.g. the client
// registered both "bash" and "Bash") is ambiguous → refuse. This is the
// privilege-escalation guard: a hallucinated/native name must not be coerced
// onto one of two equally-matching shell tools by inventory order.
func TestResolveToolSpec_AliasCandidateNormalizedCollisionRefuses(t *testing.T) {
	tools := []cursorToolDefinition{
		{Name: "bash"},
		{Name: "Bash"}, // distinct tool, same normalized form as "bash"
	}
	// "terminal" aliases to {bash, shell}; the first candidate "bash" matches two
	// distinct tools -> ambiguous.
	if spec := resolveToolSpec("terminal", tools, nil); spec != nil {
		t.Fatalf("alias candidate matching two distinct tools must refuse, routed to %q", spec.Name)
	}
}

// ADD-51: the alias candidate LIST is an intentional ordered preference (e.g.
// "terminal" -> {bash, shell} prefers bash). When the candidates map to
// DISTINCT tools, this deliberate preference is preserved (it is unambiguous per
// candidate). Only normalized collisions WITHIN a candidate are refused; the
// ordered fall-through is by design, not a nondeterminism bug.
func TestResolveToolSpec_AliasOrderedPreferencePreserved(t *testing.T) {
	tools := []cursorToolDefinition{
		{Name: "shell"}, // listed first in inventory, but bash is the alias's first preference
		{Name: "bash"},
	}
	if spec := resolveToolSpec("terminal", tools, nil); spec == nil || spec.Name != "bash" {
		t.Fatalf("alias ordered preference must pick bash regardless of inventory order; got %+v", spec)
	}
}

// ADD-51: an alias candidate that matches EXACTLY ONE client tool still resolves
// (the fix only suppresses AMBIGUOUS fuzzy matches, not unambiguous ones).
func TestResolveToolSpec_AliasSingleMatchResolves(t *testing.T) {
	tools := []cursorToolDefinition{
		{Name: "bash"},
		{Name: "Read"},
	}
	// "terminal" aliases to {bash, shell}; only bash is present -> unambiguous.
	if spec := resolveToolSpec("terminal", tools, nil); spec == nil || spec.Name != "bash" {
		t.Fatalf("single alias match must resolve to bash; got %+v", spec)
	}
}

// ADD-51: a normalized match with exactly one candidate still resolves.
func TestResolveToolSpec_NormalizedSingleMatchResolves(t *testing.T) {
	tools := []cursorToolDefinition{{Name: "read_file"}, {Name: "Bash"}}
	if spec := resolveToolSpec("ReadFile", tools, nil); spec == nil || spec.Name != "read_file" {
		t.Fatalf("single normalized match must resolve to read_file; got %+v", spec)
	}
}

// ADD-51: an explicit operator override is an intentional disambiguation and is
// allowed to bypass the ambiguity guard. It must prefer an EXACT target-name
// match so a normalized collision in the inventory cannot flip the target.
func TestResolveToolSpec_OverrideBypassesAmbiguityPrefersExact(t *testing.T) {
	tools := []cursorToolDefinition{
		{Name: "bash"},
		{Name: "shell"},
	}
	overrides := map[string]string{"terminal": "shell"}
	if spec := resolveToolSpec("terminal", tools, overrides); spec == nil || spec.Name != "shell" {
		t.Fatalf("override must route to the exact configured target 'shell'; got %+v", spec)
	}
}

// ADD-51 end-to-end: when the resolver refuses an ambiguous match,
// mapComposerToolCall must preserve the RAW emitted tool name (the client can
// recognize or reject it) rather than fabricating a route to the wrong tool.
// Two client tools with the same normalized form make the emitted (non-exact)
// name ambiguous.
func TestMapComposerToolCall_AmbiguousPreservesRawName(t *testing.T) {
	defs := []cursorToolDefinition{
		{Name: "run_cmd", Parameters: `{"type":"object","properties":{"command":{"type":"string"}}}`},
		{Name: "RunCmd", Parameters: `{"type":"object","properties":{"command":{"type":"string"}}}`},
	}
	// "run.cmd" normalizes to "runcmd" — matches both distinct tools, neither
	// exactly -> ambiguous -> raw name preserved.
	input := gjson.Parse(`{"command":"ls"}`)
	name, args := mapComposerToolCall("run.cmd", input, defs, nil)
	if name != "run.cmd" {
		t.Fatalf("ambiguous mapping must keep the raw name 'run.cmd', got %q", name)
	}
	// Args still pass through (verbatim, since no schema was applied).
	if !strings.Contains(args, `"command":"ls"`) {
		t.Fatalf("args should pass through for an unresolved tool, got %s", args)
	}
}
