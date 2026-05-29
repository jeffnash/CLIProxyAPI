package executor

import "testing"

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
