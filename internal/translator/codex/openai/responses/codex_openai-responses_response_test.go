package responses

import (
	"context"
	"strings"
	"testing"

	translatorcommon "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/common"
	"github.com/tidwall/gjson"
)

func byteChunksToStrings(chunks [][]byte) []string {
	out := make([]string, 0, len(chunks))
	for _, chunk := range chunks {
		out = append(out, string(chunk))
	}
	return out
}

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
			out = append(out, byteChunksToStrings(ConvertCodexResponseToOpenAIResponses(
				context.Background(),
				"test-model",
				nil, nil,
				[]byte(line),
				&param,
			))...)
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
		got := byteChunksToStrings(ConvertCodexResponseToOpenAIResponses(
			context.Background(),
			"test-model",
			nil, nil,
			[]byte("event: response.created"),
			nil,
		))
		if len(got) != 1 || got[0] != "event: response.created" {
			t.Fatalf("unexpected output: %v", got)
		}
	})

	t.Run("blank delimiter passes through when param is nil", func(t *testing.T) {
		got := byteChunksToStrings(ConvertCodexResponseToOpenAIResponses(
			context.Background(),
			"test-model",
			nil, nil,
			[]byte(""),
			nil,
		))
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

// TestConvertCodexResponseToOpenAIResponses_FramedDataKeepsTerminator validates
// that bridge-expanded events arriving as complete SSE frames ("data: ...\n\n")
// keep their frame terminator so downstream sees a complete event instead of
// buffering forever (regression: xai apply_patch live preview hang).
func TestConvertCodexResponseToOpenAIResponses_FramedDataKeepsTerminator(t *testing.T) {
	frame := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"+hello\\n\"}\n\n"

	t.Run("stateful param", func(t *testing.T) {
		var param any
		got := byteChunksToStrings(ConvertCodexResponseToOpenAIResponses(
			context.Background(), "test-model", nil, nil, []byte(frame), &param,
		))
		if len(got) != 1 || got[0] != frame {
			t.Fatalf("expected framed chunk preserved, got: %q", got)
		}
	})

	t.Run("nil param", func(t *testing.T) {
		got := byteChunksToStrings(ConvertCodexResponseToOpenAIResponses(
			context.Background(), "test-model", nil, nil, []byte(frame), nil,
		))
		if len(got) != 1 || got[0] != frame {
			t.Fatalf("expected framed chunk preserved, got: %q", got)
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

			hasOriginal := strings.Contains(string(result[0]), "original instructions from user")
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
		"",                        // empty line (SSE separator)
		"event: response.created", // event type line
		`data: {"type":"response.created","response":{"id":"resp_123"}}`, // data line
		"",                                  // empty line
		"event: response.output_text.delta", // event type line
		`data: {"type":"response.output_text.delta","delta":"Hello"}`, // data line
		"",                          // empty line
		": keepalive comment",       // SSE comment
		"event: response.completed", // event type line
		`data: {"type":"response.completed","response":{"id":"resp_123"}}`, // data line
		"",             // empty line
		"data: [DONE]", // done marker
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
		allResults = append(allResults, byteChunksToStrings(result)...)
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
		"data: ", // Empty data payload - should be filtered!
		"",       // SSE delimiter
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
		allResults = append(allResults, byteChunksToStrings(result)...)
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

func TestConvertCodexResponseToOpenAIResponses_CreatedIncludesOriginalRequestModel(t *testing.T) {
	request := []byte(`{"model":"original-codex-model"}`)
	translatedRequest := []byte(`{"model":"translated-codex-model"}`)
	for eventName, raw := range map[string][]byte{
		"response.created":     []byte(`data: {"type":"response.created","response":{"id":"resp_1"}}`),
		"response.in_progress": []byte(`data: {"type":"response.in_progress","response":{"id":"resp_1"}}`),
	} {
		outputs := ConvertCodexResponseToOpenAIResponses(context.Background(), "fallback-model", request, translatedRequest, raw, nil)
		if len(outputs) != 1 {
			t.Fatalf("%s outputs = %d, want 1", eventName, len(outputs))
		}
		if got := gjson.GetBytes(outputs[0], "response.model").String(); got != "original-codex-model" {
			t.Fatalf("%s models = %q, want original-codex-model; payload=%s", eventName, got, outputs[0])
		}
	}
}

func TestConvertCodexResponseToOpenAIResponsesNonStreamIncomplete(t *testing.T) {
	raw := []byte(`{"type":"response.incomplete","response":{"id":"resp_1","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`)

	out := ConvertCodexResponseToOpenAIResponsesNonStream(context.Background(), "gpt-5.5", nil, nil, raw, nil)

	if got := gjson.GetBytes(out, "status").String(); got != "incomplete" {
		t.Fatalf("status = %q, want incomplete; payload=%s", got, out)
	}
	if got := gjson.GetBytes(out, "incomplete_details.reason").String(); got != "max_output_tokens" {
		t.Fatalf("incomplete reason = %q, want max_output_tokens; payload=%s", got, out)
	}
}

func TestConvertCodexResponseToOpenAIResponses_PreservesWebSearchSources(t *testing.T) {
	const searchItem = `{"id":"ws_fixture_0123456789abcdef0123456789abcdef","type":"web_search_call","status":"completed","action":{"type":"search","queries":["python asyncio documentation"],"query":"python asyncio documentation","sources":[{"type":"url","url":"https://docs.python.org/3.13/library/asyncio.html"},{"type":"url","url":"https://docs.python.org/3/library/asyncio-task.html?highlight=n"}]}}`
	const messageItem = `{"id":"msg_fixture_0123456789abcdef0123456789abcdef","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Python docs","annotations":[{"type":"url_citation","start_index":0,"end_index":11,"title":"asyncio","url":"https://docs.python.org/3/library/asyncio.html?trk=article-ssr-frontend-pulse_little-text-block&utm_source=openai"}]}]}`

	done := []byte(`data: {"type":"response.output_item.done","output_index":0,"item":` + searchItem + "}")
	outputs := ConvertCodexResponseToOpenAIResponses(context.Background(), "gpt-6-luna", nil, nil, done, nil)
	if len(outputs) != 1 {
		t.Fatalf("output_item.done outputs = %d, want 1", len(outputs))
	}
	if got := gjson.GetBytes(outputs[0], "item.id").String(); got != "ws_fixture_0123456789abcdef0123456789abcdef" {
		t.Fatalf("item.id = %q, want ws_fixture_0123456789abcdef0123456789abcdef", got)
	}
	sources := gjson.GetBytes(outputs[0], "item.action.sources").Array()
	if len(sources) != 2 {
		t.Fatalf("item.action.sources = %s, want 2 entries", gjson.GetBytes(outputs[0], "item.action.sources").Raw)
	}
	if got := sources[0].Get("url").String(); got != "https://docs.python.org/3.13/library/asyncio.html" {
		t.Fatalf("sources[0].url = %q", got)
	}
	if got := sources[1].Get("url").String(); got != "https://docs.python.org/3/library/asyncio-task.html?highlight=n" {
		t.Fatalf("sources[1].url = %q", got)
	}

	completed := []byte(`data: {"type":"response.completed","response":{"id":"resp_04f9","status":"completed","output":[` + searchItem + "," + messageItem + "]}}")
	outputs = ConvertCodexResponseToOpenAIResponses(context.Background(), "gpt-6-luna", nil, nil, completed, nil)
	if len(outputs) != 1 {
		t.Fatalf("response.completed outputs = %d, want 1", len(outputs))
	}
	if got := gjson.GetBytes(outputs[0], "response.output.0.action.sources.1.url").String(); got != "https://docs.python.org/3/library/asyncio-task.html?highlight=n" {
		t.Fatalf("response.output.0.action.sources.1.url = %q", got)
	}
	if got := gjson.GetBytes(outputs[0], "response.output.1.id").String(); got != "msg_fixture_0123456789abcdef0123456789abcdef" {
		t.Fatalf("response.output.1.id = %q, want msg_fixture_0123456789abcdef0123456789abcdef", got)
	}
	if got := gjson.GetBytes(outputs[0], "response.output.1.content.0.annotations.0.url").String(); got != "https://docs.python.org/3/library/asyncio.html?trk=article-ssr-frontend-pulse_little-text-block&utm_source=openai" {
		t.Fatalf("citation url = %q", got)
	}
}

func TestConvertCodexResponseToOpenAIResponsesNonStream_PreservesWebSearchSources(t *testing.T) {
	raw := []byte(`{"type":"response.completed","response":{"id":"resp_04f9","status":"completed","output":[{"id":"ws_fixture_0123456789abcdef0123456789abcdef","type":"web_search_call","status":"completed","action":{"type":"search","query":"python asyncio documentation","sources":[{"type":"url","url":"https://docs.python.org/3.13/library/asyncio.html"},{"type":"url","url":"https://docs.python.org/3/library/asyncio-task.html?highlight=n"}]}},{"id":"msg_fixture_0123456789abcdef0123456789abcdef","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Python docs","annotations":[{"type":"url_citation","start_index":0,"end_index":11,"title":"asyncio","url":"https://docs.python.org/3/library/asyncio.html?trk=article-ssr-frontend-pulse_little-text-block&utm_source=openai"}]}]}]}}`)

	out := ConvertCodexResponseToOpenAIResponsesNonStream(context.Background(), "gpt-6-luna", nil, nil, raw, nil)

	if got := gjson.GetBytes(out, "output.0.id").String(); got != "ws_fixture_0123456789abcdef0123456789abcdef" {
		t.Fatalf("output.0.id = %q, want ws_fixture_0123456789abcdef0123456789abcdef", got)
	}
	sources := gjson.GetBytes(out, "output.0.action.sources").Array()
	if len(sources) != 2 {
		t.Fatalf("output.0.action.sources = %s, want 2 entries", gjson.GetBytes(out, "output.0.action.sources").Raw)
	}
	if got := sources[0].Get("url").String(); got != "https://docs.python.org/3.13/library/asyncio.html" {
		t.Fatalf("sources[0].url = %q", got)
	}
	if got := sources[1].Get("url").String(); got != "https://docs.python.org/3/library/asyncio-task.html?highlight=n" {
		t.Fatalf("sources[1].url = %q", got)
	}
	if got := gjson.GetBytes(out, "output.1.content.0.annotations.0.type").String(); got != "url_citation" {
		t.Fatalf("annotation type = %q, want url_citation", got)
	}
	if got := gjson.GetBytes(out, "output.1.content.0.annotations.0.url").String(); got != "https://docs.python.org/3/library/asyncio.html?trk=article-ssr-frontend-pulse_little-text-block&utm_source=openai" {
		t.Fatalf("citation url = %q", got)
	}
}

func TestConvertCodexResponseToOpenAIResponses_MissingSourcesStayAbsentWithCitation(t *testing.T) {
	const searchItem = `{"id":"ws_missing","type":"web_search_call","status":"completed","action":{"type":"search","query":"python asyncio documentation"}}`
	const messageItem = `{"id":"msg_cited","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Python docs","annotations":[{"type":"url_citation","start_index":0,"end_index":11,"title":"asyncio","url":"https://docs.python.org/3/library/asyncio.html?trk=article-ssr-frontend-pulse_little-text-block&utm_source=openai"}]}]}`

	done := []byte(`data: {"type":"response.output_item.done","output_index":0,"item":` + searchItem + "}")
	outputs := ConvertCodexResponseToOpenAIResponses(context.Background(), "gpt-6-luna", nil, nil, done, nil)
	if len(outputs) != 1 {
		t.Fatalf("output_item.done outputs = %d, want 1", len(outputs))
	}
	if gjson.GetBytes(outputs[0], "item.action.sources").Exists() {
		t.Fatalf("sources should not be fabricated on output_item.done: %s", outputs[0])
	}

	completed := []byte(`data: {"type":"response.completed","response":{"id":"resp_missing","status":"completed","output":[` + searchItem + "," + messageItem + "]}}")
	outputs = ConvertCodexResponseToOpenAIResponses(context.Background(), "gpt-6-luna", nil, nil, completed, nil)
	if len(outputs) != 1 {
		t.Fatalf("response.completed outputs = %d, want 1", len(outputs))
	}
	if gjson.GetBytes(outputs[0], "response.output.0.action.sources").Exists() {
		t.Fatalf("sources should not be fabricated on response.completed: %s", outputs[0])
	}
	if got := gjson.GetBytes(outputs[0], "response.output.1.content.0.annotations.0.type").String(); got != "url_citation" {
		t.Fatalf("annotation type = %q, want url_citation; payload=%s", got, outputs[0])
	}

	out := ConvertCodexResponseToOpenAIResponsesNonStream(context.Background(), "gpt-6-luna", nil, nil, []byte(`{"type":"response.completed","response":{"id":"resp_missing","status":"completed","output":[`+searchItem+","+messageItem+"]}}"), nil)
	if gjson.GetBytes(out, "output.0.action.sources").Exists() {
		t.Fatalf("sources should not be fabricated on non-stream response: %s", out)
	}
	if got := gjson.GetBytes(out, "output.1.content.0.annotations.0.url").String(); got != "https://docs.python.org/3/library/asyncio.html?trk=article-ssr-frontend-pulse_little-text-block&utm_source=openai" {
		t.Fatalf("citation url = %q", got)
	}
}

func TestApplyPatchResponsesActualRequestGatesNativeCodex(t *testing.T) {
	original := []byte(`{"tools":[{"type":"custom","name":"apply_patch"}]}`)
	bridged, errNormalize := translatorcommon.NormalizeApplyPatchResponsesRequest(original)
	if errNormalize != nil {
		t.Fatal(errNormalize)
	}
	raw := []byte(`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"a","call_id":"c","name":"apply_patch","arguments":"{\"input\":\"p\"}"}}`)
	var native any
	out := ConvertCodexResponseToOpenAIResponses(t.Context(), "m", original, original, raw, &native)
	if len(out) != 1 || string(out[0]) != string(raw) {
		t.Fatalf("native Codex changed: %s", out)
	}
	var configuredNative any
	out = ConvertCodexResponseToOpenAIResponses(t.Context(), "m", original, bridged, raw, &configuredNative)
	if len(out) != 1 || string(out[0]) != string(raw) {
		t.Fatalf("configuration enabled native bridging: %s", out)
	}
	var xai any = translatorcommon.NewApplyPatchResponsesBridge(original)
	out = ConvertCodexResponseToOpenAIResponses(t.Context(), "m", original, bridged, raw, &xai)
	if len(out) != 3 || gjson.GetBytes(out[2][6:], "item.input").String() != "p" {
		t.Fatalf("same Codex wire format bypassed bridge: %s", out)
	}
	var failed any = translatorcommon.NewApplyPatchResponsesBridge(original)
	out = ConvertCodexResponseToOpenAIResponses(t.Context(), "m", original, bridged, []byte(`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","name":"apply_patch","arguments":"{}"}}`), &failed)
	if len(out) != 1 || gjson.GetBytes(out[0][6:], "type").String() != "response.failed" || failed.(interface{ ToolInputError() error }).ToolInputError() == nil {
		t.Fatalf("failure: %s", out)
	}
}
