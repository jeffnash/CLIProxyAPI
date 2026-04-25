package helps

import (
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func TestRepairMissingReasoningContentForToolCalls_NonStream(t *testing.T) {
	auth := &cliproxyauth.Auth{
		ID: "auth-nonstream",
		Attributes: map[string]string{
			"preserve_reasoning_content": "true",
			"base_url":                   "https://api.deepseek.com",
			"passthru_routing_name":      "deepseek-v4-pro-high",
			"upstream_model":             "deepseek-v4-pro",
		},
	}
	response := []byte(`{"choices":[{"message":{"role":"assistant","content":"","reasoning_content":"need a file listing","tool_calls":[{"id":"call_list","type":"function","function":{"name":"list","arguments":"{}"}}]}}]}`)
	RecordOpenAIReasoningContentForToolCalls(auth, response)

	request := []byte(`{"messages":[{"role":"assistant","content":"","tool_calls":[{"id":"call_list","type":"function","function":{"name":"list","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_list","content":"ok"}]}`)
	got := RepairMissingReasoningContentForToolCalls(auth, request)

	if gotReasoning := gjson.GetBytes(got, "messages.0.reasoning_content").String(); gotReasoning != "need a file listing" {
		t.Fatalf("reasoning_content = %q, want cached reasoning; payload=%s", gotReasoning, got)
	}
}

func TestRepairMissingReasoningContentForToolCalls_Stream(t *testing.T) {
	auth := &cliproxyauth.Auth{
		ID: "auth-stream",
		Attributes: map[string]string{
			"preserve_reasoning_content": "true",
			"base_url":                   "https://api.deepseek.com",
			"passthru_routing_name":      "deepseek-v4-pro-xhigh",
			"upstream_model":             "deepseek-v4-pro",
		},
	}
	recorder := NewOpenAIReasoningContentStreamRecorder(auth)
	if recorder == nil {
		t.Fatal("expected stream recorder")
	}
	recorder.Observe([]byte(`data: {"choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"first "},"finish_reason":null}]}`))
	recorder.Observe([]byte(`data: {"choices":[{"index":0,"delta":{"reasoning_content":"second"},"finish_reason":null}]}`))
	recorder.Observe([]byte(`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_read","type":"function","function":{"name":"read","arguments":""}}]},"finish_reason":null}]}`))

	request := []byte(`{"messages":[{"role":"assistant","content":"","tool_calls":[{"id":"call_read","type":"function","function":{"name":"read","arguments":"{}"}}]}]}`)
	got := RepairMissingReasoningContentForToolCalls(auth, request)

	if gotReasoning := gjson.GetBytes(got, "messages.0.reasoning_content").String(); gotReasoning != "first second" {
		t.Fatalf("reasoning_content = %q, want streamed reasoning; payload=%s", gotReasoning, got)
	}
}

func TestRepairMissingReasoningContentForToolCalls_OptInOnly(t *testing.T) {
	auth := &cliproxyauth.Auth{
		ID: "auth-disabled",
		Attributes: map[string]string{
			"base_url":              "https://api.deepseek.com",
			"passthru_routing_name": "deepseek-v4-pro-high",
			"upstream_model":        "deepseek-v4-pro",
		},
	}
	response := []byte(`{"choices":[{"message":{"reasoning_content":"hidden","tool_calls":[{"id":"call_nope"}]}}]}`)
	RecordOpenAIReasoningContentForToolCalls(auth, response)

	request := []byte(`{"messages":[{"role":"assistant","tool_calls":[{"id":"call_nope"}]}]}`)
	got := RepairMissingReasoningContentForToolCalls(auth, request)

	if gjson.GetBytes(got, "messages.0.reasoning_content").Exists() {
		t.Fatalf("reasoning_content should not be added without opt-in; payload=%s", got)
	}
}
