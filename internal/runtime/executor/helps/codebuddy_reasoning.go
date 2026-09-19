package helps

import (
	"fmt"
	"strings"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// RepairCodeBuddyReasoningContent restores cached upstream reasoning_content
// onto assistant tool-call messages that lost it in a client round trip.
// Only genuine cache hits are restored: assistant messages without tool
// calls and cache misses stay untouched, never fabricated.
func RepairCodeBuddyReasoningContent(auth *cliproxyauth.Auth, payload []byte) []byte {
	if len(payload) == 0 {
		return payload
	}
	scope := reasoningContentScope(auth)
	if scope == "" {
		return payload
	}
	messages := gjson.GetBytes(payload, "messages")
	if !messages.IsArray() {
		return payload
	}
	out := payload
	for i, msg := range messages.Array() {
		if !strings.EqualFold(strings.TrimSpace(msg.Get("role").String()), "assistant") {
			continue
		}
		if rc := msg.Get("reasoning_content"); rc.Exists() && strings.TrimSpace(rc.String()) != "" {
			continue
		}
		toolCalls := msg.Get("tool_calls")
		if !toolCalls.IsArray() || len(toolCalls.Array()) == 0 {
			continue
		}
		reasoning := reasoningContentForToolCalls(scope, toolCalls)
		if reasoning == "" {
			continue
		}
		updated, errSet := sjson.SetBytes(out, fmt.Sprintf("messages.%d.reasoning_content", i), reasoning)
		if errSet == nil {
			out = updated
		}
	}
	return out
}

// RecordCodeBuddyReasoningContent caches reasoning_content from an aggregated
// OpenAI-compatible completion, keyed by tool call ID for later turns.
func RecordCodeBuddyReasoningContent(auth *cliproxyauth.Auth, completion []byte) {
	if len(completion) == 0 {
		return
	}
	scope := reasoningContentScope(auth)
	if scope == "" {
		return
	}
	recordOpenAIReasoningContentForToolCalls(scope, completion)
}

// NewCodeBuddyReasoningStreamRecorder creates a stream recorder for CodeBuddy
// responses. Preservation is native provider behavior here, not a per-route
// opt-in, so this bypasses the attribute gate of the generic constructor.
func NewCodeBuddyReasoningStreamRecorder(auth *cliproxyauth.Auth) *OpenAIReasoningContentStreamRecorder {
	return newOpenAIReasoningContentStreamRecorderWithScope(reasoningContentScope(auth))
}
