package executor

import (
	"encoding/json"
	"testing"
)

func TestRepairChutesJSONString_ValidJSON(t *testing.T) {
	input := `{"key": "value"}`
	result := repairChutesJSONString(input)
	if result != input {
		t.Errorf("expected %q, got %q", input, result)
	}
}

func TestRepairChutesJSONString_UnescapedNewline(t *testing.T) {
	// Simulate a malformed JSON string with literal newline inside
	input := "{\"content\": \"line1\nline2\"}"
	result := repairChutesJSONString(input)
	
	// Verify the result is valid JSON
	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Errorf("repaired JSON should be valid, got error: %v, result: %q", err, result)
	}
}

func TestRepairChutesJSONString_UnescapedTab(t *testing.T) {
	input := "{\"content\": \"col1\tcol2\"}"
	result := repairChutesJSONString(input)
	
	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Errorf("repaired JSON should be valid, got error: %v, result: %q", err, result)
	}
}

func TestRepairChutesToolCallArguments_NoToolCalls(t *testing.T) {
	input := []byte(`data: {"choices":[{"delta":{"content":"hello"}}]}`)
	result := repairChutesToolCallArguments(input)
	if string(result) != string(input) {
		t.Errorf("expected unchanged input for non-tool-call response")
	}
}

func TestRepairChutesToolCallArguments_ValidToolCall(t *testing.T) {
	input := []byte(`data: {"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"{\"key\":\"value\"}"}}]}}]}`)
	result := repairChutesToolCallArguments(input)
	if string(result) != string(input) {
		t.Errorf("expected unchanged input for valid tool call")
	}
}

func TestRepairChutesNonStreamToolCallArguments_NoToolCalls(t *testing.T) {
	input := []byte(`{"choices":[{"message":{"content":"hello"}}]}`)
	result := repairChutesNonStreamToolCallArguments(input)
	if string(result) != string(input) {
		t.Errorf("expected unchanged input for non-tool-call response")
	}
}

func TestRepairChutesNonStreamToolCallArguments_ValidToolCall(t *testing.T) {
	input := []byte(`{"choices":[{"message":{"tool_calls":[{"function":{"arguments":"{\"key\":\"value\"}"}}]}}]}`)
	result := repairChutesNonStreamToolCallArguments(input)
	if string(result) != string(input) {
		t.Errorf("expected unchanged input for valid tool call")
	}
}
