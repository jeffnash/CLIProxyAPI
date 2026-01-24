package gemini

import (
	"context"
	"testing"
)

// TestPassthroughGeminiResponseStream_DataHandling validates that
// the translator correctly handles various input types.
func TestPassthroughGeminiResponseStream_DataHandling(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		wantLen    int
		wantOutput string
	}{
		// Empty and whitespace inputs should be filtered
		{
			name:    "empty string is filtered",
			input:   "",
			wantLen: 0,
		},
		{
			name:    "CR-only line is filtered",
			input:   "\r",
			wantLen: 0,
		},
		{
			name:    "data prefix with empty payload is filtered",
			input:   "data: ",
			wantLen: 0,
		},
		{
			name:    "data prefix with empty payload and trailing CR is filtered",
			input:   "data: \r",
			wantLen: 0,
		},
		{
			name:    "data prefix with whitespace payload is filtered",
			input:   "data:   ",
			wantLen: 0,
		},
		{
			name:    "data prefix with newline is filtered",
			input:   "data: \n",
			wantLen: 0,
		},
		// [DONE] marker should be filtered
		{
			name:    "DONE marker is filtered",
			input:   "data: [DONE]",
			wantLen: 0,
		},
		{
			name:    "raw DONE marker is filtered",
			input:   "[DONE]",
			wantLen: 0,
		},
		// Valid JSON payloads should pass through (with data: prefix stripped)
		{
			name:       "valid JSON payload passes through",
			input:      `data: {"candidates":[{"content":{"parts":[{"text":"Hello"}]}}]}`,
			wantLen:    1,
			wantOutput: `{"candidates":[{"content":{"parts":[{"text":"Hello"}]}}]}`,
		},
		{
			name:       "raw JSON without data prefix passes through",
			input:      `{"candidates":[{"content":{"parts":[{"text":"Hi"}]}}]}`,
			wantLen:    1,
			wantOutput: `{"candidates":[{"content":{"parts":[{"text":"Hi"}]}}]}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := PassthroughGeminiResponseStream(
				context.Background(),
				"test-model",
				nil, nil,
				[]byte(tc.input),
				nil,
			)

			if len(result) != tc.wantLen {
				t.Errorf("expected %d results, got %d: %v", tc.wantLen, len(result), result)
				return
			}

			if tc.wantLen > 0 && result[0] != tc.wantOutput {
				t.Errorf("expected output %q, got %q", tc.wantOutput, result[0])
			}
		})
	}
}

// TestPassthroughGeminiResponseStream_DataPrefixStripping validates that
// the "data: " prefix is properly stripped from the output.
func TestPassthroughGeminiResponseStream_DataPrefixStripping(t *testing.T) {
	input := `data: {"candidates":[{"content":{"parts":[{"text":"Test"}]}}]}`
	expected := `{"candidates":[{"content":{"parts":[{"text":"Test"}]}}]}`

	result := PassthroughGeminiResponseStream(
		context.Background(),
		"test-model",
		nil, nil,
		[]byte(input),
		nil,
	)

	if len(result) != 1 {
		t.Fatalf("expected 1 result, got %d", len(result))
	}

	if result[0] != expected {
		t.Errorf("expected data prefix to be stripped\ngot:  %s\nwant: %s", result[0], expected)
	}
}

// TestPassthroughGeminiResponseStream_SSEStreamSimulation simulates
// a realistic stream with various payload types.
func TestPassthroughGeminiResponseStream_SSEStreamSimulation(t *testing.T) {
	// Simulate stream chunks as they arrive
	// Note: Gemini streams are typically JSON chunks, possibly with data: prefix
	// The handler adds proper SSE framing
	streamChunks := []string{
		`data: {"candidates":[{"content":{"parts":[{"text":""}]}}]}`,
		"data: ",                    // empty payload (should be filtered)
		`data: {"candidates":[{"content":{"parts":[{"text":"Hello"}]}}]}`,
		"data:   ",                  // whitespace only (should be filtered)
		`data: {"candidates":[{"content":{"parts":[{"text":" world"}]}}]}`,
		"",                          // empty string (should be filtered)
		`data: {"candidates":[{"content":{"parts":[{"text":"!"}]}}],"usageMetadata":{}}`,
		"data: [DONE]",              // DONE marker (should be filtered)
	}

	var validResults []string
	for _, chunk := range streamChunks {
		result := PassthroughGeminiResponseStream(
			context.Background(),
			"test-model",
			nil, nil,
			[]byte(chunk),
			nil,
		)
		validResults = append(validResults, result...)
	}

	// We expect 4 valid JSON results to pass through
	// (the 4 JSON chunks; empty data, whitespace, empty string, and [DONE] filtered)
	expectedCount := 4
	if len(validResults) != expectedCount {
		t.Errorf("expected %d valid results, got %d", expectedCount, len(validResults))
		for i, r := range validResults {
			t.Logf("  result[%d]: %s", i, r)
		}
	}

	// Verify no result has "data: " prefix (should be stripped)
	for i, r := range validResults {
		if len(r) >= 6 && r[:6] == "data: " {
			t.Errorf("result[%d] still has 'data: ' prefix: %s", i, r)
		}
	}
}

// TestPassthroughGeminiResponseNonStream validates non-streaming passthrough.
func TestPassthroughGeminiResponseNonStream(t *testing.T) {
	input := `{"candidates":[{"content":{"parts":[{"text":"Complete response"}]}}]}`

	result := PassthroughGeminiResponseNonStream(
		context.Background(),
		"test-model",
		nil, nil,
		[]byte(input),
		nil,
	)

	if result != input {
		t.Errorf("expected passthrough to return input unchanged\ngot:  %s\nwant: %s", result, input)
	}
}

// TestGeminiTokenCount validates the token count response format.
func TestGeminiTokenCount(t *testing.T) {
	result := GeminiTokenCount(context.Background(), 42)
	expected := `{"totalTokens":42,"promptTokensDetails":[{"modality":"TEXT","tokenCount":42}]}`

	if result != expected {
		t.Errorf("unexpected token count response\ngot:  %s\nwant: %s", result, expected)
	}
}
