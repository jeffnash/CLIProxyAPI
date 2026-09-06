package secretdlp

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Opaque collaboration identifiers must round-trip through Secret-DLP restore
// even when a scanner treated them as secrets. Restoration is path-unrestricted:
// nested values, object keys, tool arguments, schemas, and split streaming chunks
// must not leak unresolved __CPA_DLP_v1_ placeholders to the client.
func TestSessionRestoresCollaborationIdentifiersInValuesKeysArgsSchemasAndStream(t *testing.T) {
	session := NewSession([]byte("master-key"), "client-key", time.Minute, ModeRestore)

	ids := []struct {
		field string
		value string
	}{
		{"session_id", "sess-4d45ec6e041b4fc2a2212bcc64b3bb10"},
		{"participant_id", "part-8c91a2b3c4d5e6f708192a3b4c5d6e7f"},
		{"message_id", "msg-a1b2c3d4e5f60718293a4b5c6d7e8f90"},
		{"evidence_id", "evid-b2c3d4e5f60718293a4b5c6d7e8f90a1"},
		{"artifact_id", "artf-c3d4e5f60718293a4b5c6d7e8f90a1b2"},
		{"run_id", "run-d4e5f60718293a4b5c6d7e8f90a1b2c3"},
		{"call_id", "call_e5f60718293a4b5c6d7e8f90a1b2c3d4"},
	}

	placeholders := make(map[string]string, len(ids))
	values := make(map[string]string, len(ids))
	for _, id := range ids {
		redacted := redactRawForTest(t, session, []byte(id.value), []Finding{{Secret: id.value, RuleID: "test", Source: "test"}})
		placeholders[id.field] = extractPlaceholderForTest(t, string(redacted))
		values[id.field] = id.value
	}

	args, err := json.Marshal(map[string]any{
		"session_id":           placeholders["session_id"],
		"participant_id":       placeholders["participant_id"],
		"message_id":           placeholders["message_id"],
		"evidence_id":          placeholders["evidence_id"],
		"artifact_id":          placeholders["artifact_id"],
		"run_id":               placeholders["run_id"],
		placeholders["run_id"]: "tool-arg-object-key",
	})
	if err != nil {
		t.Fatalf("json.Marshal(tool args): %v", err)
	}

	payload := map[string]any{
		"session_id":               placeholders["session_id"],
		"participant_id":           placeholders["participant_id"],
		"call_id":                  placeholders["call_id"],
		"tool_call_id":             placeholders["call_id"],
		placeholders["session_id"]: map[string]any{"via": "object-key"},
		"nested": map[string]any{
			"message_id":                   placeholders["message_id"],
			"evidence_id":                  placeholders["evidence_id"],
			"artifact_id":                  placeholders["artifact_id"],
			"run_id":                       placeholders["run_id"],
			placeholders["participant_id"]: "nested-object-key",
		},
		"choices": []any{
			map[string]any{
				"message": map[string]any{
					"tool_calls": []any{
						map[string]any{
							"id": placeholders["call_id"],
							"function": map[string]any{
								"arguments": string(args),
							},
						},
					},
				},
			},
		},
		"tools": []any{
			map[string]any{
				"type": "function",
				"function": map[string]any{
					"parameters": map[string]any{
						"type": "object",
						"properties": map[string]any{
							placeholders["artifact_id"]: map[string]any{"type": "string"},
						},
						"required": []any{placeholders["artifact_id"]},
					},
				},
			},
		},
	}

	wire, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("json.Marshal(payload): %v", err)
	}
	if !strings.Contains(string(wire), placeholderPrefix) {
		t.Fatal("redacted payload missing DLP placeholder before restore")
	}

	restored := session.RestoreJSON(wire)
	assertCollaborationIDsRestored(t, restored, ids, placeholders, "structured")

	root := decodeJSONForTest(t, restored)
	if _, ok := root[values["session_id"]]; !ok {
		t.Fatalf("restored object keys = %v, want session_id value restored as object key", keysOf(root))
	}
	nested := root["nested"].(map[string]any)
	if got := nested["message_id"]; got != values["message_id"] {
		t.Fatalf("nested message_id = %v, want %q", got, values["message_id"])
	}
	if _, ok := nested[values["participant_id"]]; !ok {
		t.Fatalf("nested object keys = %v, want participant_id value restored as object key", keysOf(nested))
	}

	message := root["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	toolCall := message["tool_calls"].([]any)[0].(map[string]any)
	if got := toolCall["id"]; got != values["call_id"] {
		t.Fatalf("tool_calls.id = %v, want %q", got, values["call_id"])
	}
	var inner map[string]any
	if err := json.Unmarshal([]byte(toolCall["function"].(map[string]any)["arguments"].(string)), &inner); err != nil {
		t.Fatalf("tool arguments JSON after restore: %v", err)
	}
	if got := inner["evidence_id"]; got != values["evidence_id"] {
		t.Fatalf("tool arg evidence_id = %v, want %q", got, values["evidence_id"])
	}
	if _, ok := inner[values["run_id"]]; !ok {
		t.Fatalf("tool arg object keys = %v, want run_id value restored as object key", keysOf(inner))
	}

	properties := root["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)["parameters"].(map[string]any)["properties"].(map[string]any)
	if _, ok := properties[values["artifact_id"]]; !ok {
		t.Fatalf("schema properties = %v, want artifact_id restored as object key", keysOf(properties))
	}
	required := root["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)["parameters"].(map[string]any)["required"].([]any)
	if len(required) != 1 || required[0] != values["artifact_id"] {
		t.Fatalf("schema required = %v, want restored artifact_id", required)
	}

	splitAt := placeholders["session_id"]
	mid := len(splitAt) / 2
	sse := append(append([]byte("data: "), wire...), []byte("\n\n")...)
	idx := strings.Index(string(sse), splitAt)
	if idx < 0 {
		t.Fatal("sse payload missing session_id placeholder to split")
	}
	first := sse[:idx+mid]
	second := sse[idx+mid:]

	var out []byte
	out = append(out, session.RestoreStreamJSONChunk(first)...)
	out = append(out, session.RestoreStreamJSONChunk(second)...)
	out = append(out, session.FlushStreamJSONTail()...)
	assertCollaborationIDsRestored(t, out, ids, placeholders, "split-stream")
}

func assertCollaborationIDsRestored(t *testing.T, body []byte, ids []struct {
	field string
	value string
}, placeholders map[string]string, path string) {
	t.Helper()
	got := string(body)
	if strings.Contains(got, placeholderPrefix) {
		t.Fatalf("%s restore still contains unresolved DLP placeholder: %s", path, got)
	}
	for _, id := range ids {
		if strings.Contains(got, placeholders[id.field]) {
			t.Fatalf("%s restore still contains placeholder for %s", path, id.field)
		}
		if !strings.Contains(got, id.value) {
			t.Fatalf("%s restore missing %s %q in %s", path, id.field, id.value, got)
		}
	}
}

func keysOf(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	return keys
}
