package responses

import (
	"context"
	"strings"
	"testing"
)

// TestConvertCodexResponseToOpenAIResponses_SSELineHandling validates the translator's
// stateful handling of various SSE line types. It should:
// - Filter empty "data:" payloads (to prevent JSON parse errors)
// - Buffer "event:" lines and only emit them with a subsequent non-empty "data:" payload
// - Only emit delimiter/blank lines after a non-empty "data:" payload has been emitted
// - Pass through non-event, non-data lines unchanged
func TestConvertCodexResponseToOpenAIResponses_SSELineHandling(t *testing.T) {
	run := func(lines ...string) []string {
		var param any
		var out []string
		for _, line := range lines {
			out = append(out, ConvertCodexResponseToOpenAIResponses(
				context.Background(),
				"test-model",
				nil, nil,
				[]byte(line),
				&param,
			)...)
		}
		return out
	}

	t.Run("delimiter suppressed before any non-empty data", func(t *testing.T) {
		if got := run("", "\r"); len(got) != 0 {
			t.Fatalf("expected no output, got: %v", got)
		}
	})

	t.Run("event lines are buffered until non-empty data arrives", func(t *testing.T) {
		got := run("event: response.created", `data: {"type":"response.created","response":{}}`)
		if len(got) != 2 {
			t.Fatalf("expected 2 outputs (event+data), got %d: %v", len(got), got)
		}
		if got[0] != "event: response.created" {
			t.Fatalf("expected buffered event line first, got %q", got[0])
		}
		if got[1] != `data: {"type":"response.created","response":{}}` {
			t.Fatalf("expected data line second, got %q", got[1])
		}
	})

	t.Run("event line is suppressed if followed by empty data payload", func(t *testing.T) {
		got := run("event: some_event", "data: ", "", `data: {"valid":true}`)
		if len(got) != 1 {
			t.Fatalf("expected only the valid data to pass through, got %d: %v", len(got), got)
		}
		if got[0] != `data: {"valid":true}` {
			t.Fatalf("expected valid data output, got %q", got[0])
		}
	})

	t.Run("delimiter is emitted after non-empty data", func(t *testing.T) {
		got := run(`data: {"ok":true}`, "")
		if len(got) != 2 {
			t.Fatalf("expected data+delimiter, got %d: %v", len(got), got)
		}
		if got[0] != `data: {"ok":true}` || got[1] != "" {
			t.Fatalf("unexpected output: %v", got)
		}
	})

	t.Run("non-data, non-event lines pass through unchanged", func(t *testing.T) {
		got := run(": this is a comment", "id: msg_123", "retry: 3000", "   ")
		if len(got) != 4 {
			t.Fatalf("expected 4 passthrough lines, got %d: %v", len(got), got)
		}
		if got[0] != ": this is a comment" || got[1] != "id: msg_123" || got[2] != "retry: 3000" || got[3] != "   " {
			t.Fatalf("unexpected passthrough output: %v", got)
		}
	})
}

func TestConvertCodexResponseToOpenAIResponses_NilParamState_PassthroughFraming(t *testing.T) {
	t.Run("event line passes through when param is nil", func(t *testing.T) {
		got := ConvertCodexResponseToOpenAIResponses(
			context.Background(),
			"test-model",
			nil, nil,
			[]byte("event: response.created"),
			nil,
		)
		if len(got) != 1 || got[0] != "event: response.created" {
			t.Fatalf("unexpected output: %v", got)
		}
	})

	t.Run("blank delimiter passes through when param is nil", func(t *testing.T) {
		got := ConvertCodexResponseToOpenAIResponses(
			context.Background(),
			"test-model",
			nil, nil,
			[]byte(""),
			nil,
		)
		if len(got) != 1 || got[0] != "" {
			t.Fatalf("unexpected output: %v", got)
		}
	})

	t.Run("empty data payload is still filtered when param is nil", func(t *testing.T) {
		got := ConvertCodexResponseToOpenAIResponses(
			context.Background(),
			"test-model",
			nil, nil,
			[]byte("data: "),
			nil,
		)
		if len(got) != 0 {
			t.Fatalf("expected empty output, got: %v", got)
		}
	})
}

// TestConvertCodexResponseToOpenAIResponses_InstructionsPassthrough validates
// that instructions are properly echoed back in response events.
func TestConvertCodexResponseToOpenAIResponses_InstructionsPassthrough(t *testing.T) {
	originalReq := []byte(`{"instructions":"original instructions from user"}`)
	modifiedReq := []byte(`{"instructions":"modified instructions by proxy"}`)
	var param any

	tests := []struct {
		name       string
		eventType  string
		wantEchoed bool
	}{
		{
			name:       "response.created echoes instructions",
			eventType:  "response.created",
			wantEchoed: true,
		},
		{
			name:       "response.in_progress echoes instructions",
			eventType:  "response.in_progress",
			wantEchoed: true,
		},
		{
			name:       "response.completed echoes instructions",
			eventType:  "response.completed",
			wantEchoed: true,
		},
		{
			name:       "response.output_text.delta does not modify",
			eventType:  "response.output_text.delta",
			wantEchoed: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			input := []byte(`data: {"type":"` + tc.eventType + `","response":{"instructions":"proxy instructions"}}`)

			result := ConvertCodexResponseToOpenAIResponses(
				context.Background(),
				"test-model",
				originalReq, modifiedReq,
				input,
				&param,
			)

			if len(result) != 1 {
				t.Fatalf("expected 1 result, got %d", len(result))
			}

			hasOriginal := strings.Contains(result[0], "original instructions from user")
			if tc.wantEchoed && !hasOriginal {
				t.Errorf("expected original instructions to be echoed, got: %s", result[0])
			}
		})
	}
}

// TestConvertCodexResponseToOpenAIResponses_SSEStreamSimulation simulates
// a realistic SSE stream with mixed line types to ensure proper handling.
// The translator should preserve SSE structure while filtering problematic content.
func TestConvertCodexResponseToOpenAIResponses_SSEStreamSimulation(t *testing.T) {
	// Simulate a real SSE stream as it would arrive from upstream
	sseLines := []string{
		"",                                                                     // empty line (SSE separator)
		"event: response.created",                                              // event type line
		`data: {"type":"response.created","response":{"id":"resp_123"}}`,       // data line
		"",                                                                     // empty line
		"event: response.output_text.delta",                                    // event type line
		`data: {"type":"response.output_text.delta","delta":"Hello"}`,          // data line
		"",                                                                     // empty line
		": keepalive comment",                                                  // SSE comment
		"event: response.completed",                                            // event type line
		`data: {"type":"response.completed","response":{"id":"resp_123"}}`,     // data line
		"",                                                                     // empty line
		"data: [DONE]",                                                         // done marker
	}

	var allResults []string
	var param any
	for _, line := range sseLines {
		result := ConvertCodexResponseToOpenAIResponses(
			context.Background(),
			"test-model",
			nil, nil,
			[]byte(line),
			&param,
		)
		allResults = append(allResults, result...)
	}

	// Count by type
	var emptyLines, eventLines, commentLines, dataLines int
	for _, r := range allResults {
		switch {
		case r == "":
			emptyLines++
		case strings.HasPrefix(r, "event:"):
			eventLines++
		case strings.HasPrefix(r, ":"):
			commentLines++
		case strings.HasPrefix(r, "data:"):
			dataLines++
		}
	}

	// Verify counts
	if emptyLines != 3 {
		t.Errorf("expected 3 empty lines (SSE delimiters), got %d", emptyLines)
	}
	if eventLines != 3 {
		t.Errorf("expected 3 event lines, got %d", eventLines)
	}
	if commentLines != 1 {
		t.Errorf("expected 1 comment line, got %d", commentLines)
	}
	if dataLines != 4 {
		t.Errorf("expected 4 data lines, got %d", dataLines)
	}

	// Total should be 11: initial delimiter is suppressed until the first non-empty data payload.
	expectedTotal := len(sseLines) - 1
	if len(allResults) != expectedTotal {
		t.Errorf("expected %d total results, got %d", expectedTotal, len(allResults))
		for i, r := range allResults {
			t.Logf("  result[%d]: %q", i, r)
		}
	}
}

// TestConvertCodexResponseToOpenAIResponses_EmptyDataFiltering verifies that
// empty data: payloads are filtered out to prevent JSON parse errors downstream.
func TestConvertCodexResponseToOpenAIResponses_EmptyDataFiltering(t *testing.T) {
	// Simulate a stream with an empty data: line (would cause JSONDecodeError)
	sseLines := []string{
		"event: some_event",
		"data: ",              // Empty data payload - should be filtered!
		"",                    // SSE delimiter
		`data: {"valid":true}`,
		"",
	}

	var allResults []string
	var param any
	for _, line := range sseLines {
		result := ConvertCodexResponseToOpenAIResponses(
			context.Background(),
			"test-model",
			nil, nil,
			[]byte(line),
			&param,
		)
		allResults = append(allResults, result...)
	}

	// Should have: data line, empty line = 2 results.
	// The pending event line, empty data payload, and delimiter after the empty payload are all suppressed.
	expectedCount := 2
	if len(allResults) != expectedCount {
		t.Errorf("expected %d results (empty data: filtered), got %d", expectedCount, len(allResults))
		for i, r := range allResults {
			t.Logf("  result[%d]: %q", i, r)
		}
	}

	// Verify no result is just "data: " or "data:" (empty payload)
	for i, r := range allResults {
		trimmed := strings.TrimSpace(strings.TrimPrefix(r, "data:"))
		if strings.HasPrefix(r, "data:") && trimmed == "" {
			t.Errorf("result[%d] is empty data line which should be filtered: %q", i, r)
		}
	}
}
