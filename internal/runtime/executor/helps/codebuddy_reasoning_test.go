package helps

import (
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func codeBuddyReasoningTestAuth(id string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{ID: id, Provider: "codebuddy"}
}

func TestRepairCodeBuddyReasoningContent(t *testing.T) {
	auth := codeBuddyReasoningTestAuth("codebuddy-reasoning-native")
	completion := []byte(`{"choices":[{"message":{"role":"assistant","content":"","reasoning_content":"trace for call_rc_1","tool_calls":[{"id":"call_rc_1","type":"function","function":{"name":"f","arguments":"{}"}}]}}]}`)
	RecordCodeBuddyReasoningContent(auth, completion)

	stripped := []byte(`{"messages":[
		{"role":"user","content":"go"},
		{"role":"assistant","content":"","tool_calls":[{"id":"call_rc_1","type":"function","function":{"name":"f","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"call_rc_1","content":"ok"}
	]}`)
	got := RepairCodeBuddyReasoningContent(auth, stripped)
	if rc := gjson.GetBytes(got, "messages.1.reasoning_content").String(); rc != "trace for call_rc_1" {
		t.Fatalf("reasoning_content = %q, want cached trace; payload=%s", rc, got)
	}

	t.Run("cache miss stays absent", func(t *testing.T) {
		other := codeBuddyReasoningTestAuth("codebuddy-reasoning-miss")
		unknown := []byte(`{"messages":[{"role":"assistant","content":"text","tool_calls":[{"id":"call_never_seen"}]}]}`)
		if got := RepairCodeBuddyReasoningContent(other, unknown); gjson.GetBytes(got, "messages.0.reasoning_content").Exists() {
			t.Fatalf("fabricated reasoning_content: %s", got)
		}
	})

	t.Run("plain assistant untouched", func(t *testing.T) {
		plain := []byte(`{"messages":[{"role":"assistant","content":"Earlier answer."}]}`)
		if got := RepairCodeBuddyReasoningContent(auth, plain); gjson.GetBytes(got, "messages.0.reasoning_content").Exists() {
			t.Fatalf("non-tool assistant gained reasoning_content: %s", got)
		}
	})

	t.Run("existing trace preserved", func(t *testing.T) {
		kept := []byte(`{"messages":[{"role":"assistant","reasoning_content":"client trace","tool_calls":[{"id":"call_rc_1"}]}]}`)
		got := RepairCodeBuddyReasoningContent(auth, kept)
		if rc := gjson.GetBytes(got, "messages.0.reasoning_content").String(); rc != "client trace" {
			t.Fatalf("reasoning_content = %q, want client trace", rc)
		}
	})
}

func TestCodeBuddyReasoningStreamRecorder(t *testing.T) {
	auth := codeBuddyReasoningTestAuth("codebuddy-reasoning-stream")
	recorder := NewCodeBuddyReasoningStreamRecorder(auth)
	if recorder == nil {
		t.Fatal("expected recorder without opt-in attributes")
	}
	recorder.Observe([]byte(`data: {"choices":[{"index":0,"delta":{"reasoning_content":"streamed trace"},"finish_reason":null}]}`))
	recorder.Observe([]byte(`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_rc_stream"}]},"finish_reason":null}]}`))

	stripped := []byte(`{"messages":[{"role":"assistant","tool_calls":[{"id":"call_rc_stream"}]}]}`)
	got := RepairCodeBuddyReasoningContent(auth, stripped)
	if rc := gjson.GetBytes(got, "messages.0.reasoning_content").String(); rc != "streamed trace" {
		t.Fatalf("reasoning_content = %q, want streamed trace; payload=%s", rc, got)
	}
}
