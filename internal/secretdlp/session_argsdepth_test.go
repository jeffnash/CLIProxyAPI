package secretdlp

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// toolArgsWire mirrors the shape of an OpenAI tool-call chunk far enough to reach
// choices[].delta.tool_calls[].function.arguments, which is JSON encoded *as a string*.
type toolArgsWire struct {
	Choices []struct {
		Delta struct {
			ToolCalls []struct {
				Function struct {
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
}

func argumentsFromWire(t *testing.T, body []byte) string {
	t.Helper()
	var w toolArgsWire
	if err := json.Unmarshal(body, &w); err != nil {
		t.Fatalf("outer frame is not valid JSON: %v\n%s", err, body)
	}
	if len(w.Choices) == 0 || len(w.Choices[0].Delta.ToolCalls) == 0 {
		t.Fatalf("wire missing tool_calls: %s", body)
	}
	return w.Choices[0].Delta.ToolCalls[0].Function.Arguments
}

// A secret that needs JSON escaping (quote + newline, i.e. a PEM-shaped key) echoed back
// inside a tool call's "arguments" must round-trip: the outer frame stays valid JSON AND
// the inner arguments JSON stays valid and carries the real secret. Before the fix the
// restore single-escaped where the nesting needed two levels, corrupting the inner JSON.
func TestSessionRestoresEscapingSecretInsideToolCallArguments(t *testing.T) {
	session := NewSession([]byte("master-key"), "client-key", time.Minute, ModeRestore)
	secret := "-----BEGIN KEY-----\nA\"B\\C\n-----END KEY-----"
	redacted := redactRawForTest(t, session, []byte("raw "+secret), []Finding{{Secret: secret, RuleID: "test", Source: "test"}})
	placeholder := extractPlaceholderForTest(t, string(redacted))

	// arguments is JSON-inside-a-string: {"cmd":"<placeholder>"}
	wire := `{"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"{\"cmd\":\"` + placeholder + `\"}"}}]}}]}`
	restored := session.RestoreJSON([]byte(wire))

	if strings.Contains(string(restored), placeholder) {
		t.Fatalf("restored still contains placeholder: %s", restored)
	}

	args := argumentsFromWire(t, restored)
	var inner map[string]any
	if err := json.Unmarshal([]byte(args), &inner); err != nil {
		t.Fatalf("inner arguments JSON is corrupted after restore: %v\narguments=%q", err, args)
	}
	if inner["cmd"] != secret {
		t.Fatalf("inner cmd = %q, want restored secret %q", inner["cmd"], secret)
	}
}

// The common case (an alphanumeric API key echoed into arguments) must keep working on
// both the structured and streaming paths — this is the scenario the feature is built for.
func TestSessionRestoresAlphanumericSecretInsideToolCallArguments(t *testing.T) {
	secret := "sk-testdlpfixture0000000000000000000000000000"

	structured := NewSession([]byte("master-key"), "client-key", time.Minute, ModeRestore)
	redacted := redactRawForTest(t, structured, []byte("raw "+secret), []Finding{{Secret: secret, RuleID: "test", Source: "test"}})
	placeholder := extractPlaceholderForTest(t, string(redacted))
	wire := `{"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"{\"cmd\":\"` + placeholder + `\"}"}}]}}]}`

	restored := structured.RestoreJSON([]byte(wire))
	if inner := argumentsFromWire(t, restored); !strings.Contains(inner, secret) {
		t.Fatalf("structured restore of arguments = %q, want secret %q", inner, secret)
	}

	// Streaming path: whole frame in one chunk. Each session mints its own placeholder,
	// so build this frame from the stream session's placeholder.
	stream := NewSession([]byte("master-key"), "client-key", time.Minute, ModeRestore)
	streamRedacted := redactRawForTest(t, stream, []byte("raw "+secret), []Finding{{Secret: secret, RuleID: "test", Source: "test"}})
	streamPlaceholder := extractPlaceholderForTest(t, string(streamRedacted))
	streamWire := `data: {"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"{\"cmd\":\"` + streamPlaceholder + `\"}"}}]}}]}` + "\n\n"
	var out []byte
	out = append(out, stream.RestoreStreamJSONChunk([]byte(streamWire))...)
	out = append(out, stream.FlushStreamJSONTail()...)
	if !strings.Contains(string(out), secret) {
		t.Fatalf("streaming restore = %q, want secret %q", out, secret)
	}
}
