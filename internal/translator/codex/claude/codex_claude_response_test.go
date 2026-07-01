package claude

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertCodexResponseToClaude_StreamThinkingIncludesSignature(t *testing.T) {
	ctx := context.Background()
	originalRequest := []byte(`{"messages":[]}`)
	var param any

	chunks := [][]byte{
		[]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_123\",\"model\":\"gpt-5\"}}"),
		[]byte("data: {\"type\":\"response.reasoning_summary_part.added\"}"),
		[]byte("data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"Let me think\"}"),
		[]byte("data: {\"type\":\"response.reasoning_summary_part.done\"}"),
		[]byte("data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"reasoning\",\"encrypted_content\":\"enc_sig_123\"}}"),
	}

	var outputs [][]byte
	for _, chunk := range chunks {
		outputs = append(outputs, ConvertCodexResponseToClaude(ctx, "", originalRequest, nil, chunk, &param)...)
	}

	startFound := false
	signatureDeltaFound := false
	stopFound := false

	for _, out := range outputs {
		for _, line := range strings.Split(string(out), "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := gjson.Parse(strings.TrimPrefix(line, "data: "))
			switch data.Get("type").String() {
			case "content_block_start":
				if data.Get("content_block.type").String() == "thinking" {
					startFound = true
					if data.Get("content_block.signature").Exists() {
						t.Fatalf("thinking start block should NOT have signature field when signature is unknown: %s", line)
					}
				}
			case "content_block_delta":
				if data.Get("delta.type").String() == "signature_delta" {
					signatureDeltaFound = true
					if got := data.Get("delta.signature").String(); got != "enc_sig_123" {
						t.Fatalf("unexpected signature delta: %q", got)
					}
				}
			case "content_block_stop":
				stopFound = true
			}
		}
	}

	if !startFound {
		t.Fatal("expected thinking content_block_start event")
	}
	if !signatureDeltaFound {
		t.Fatal("expected signature_delta event for thinking block")
	}
	if !stopFound {
		t.Fatal("expected content_block_stop event for thinking block")
	}
}

func TestConvertCodexResponseToClaude_StreamCyberPolicyError(t *testing.T) {
	ctx := context.Background()
	var param any

	outputs := ConvertCodexResponseToClaude(ctx, "", []byte(`{"messages":[]}`), nil, []byte(`data: {"type":"error","error":{"type":"invalid_request","code":"cyber_policy","message":"This content was flagged for possible cybersecurity risk.","param":null},"sequence_number":3}`), &param)
	if len(outputs) != 1 {
		t.Fatalf("expected one error chunk, got %d: %q", len(outputs), outputs)
	}
	out := string(outputs[0])
	if !strings.Contains(out, "event: error\n") {
		t.Fatalf("expected Claude SSE error event, got: %q", out)
	}

	payload, ok := firstClaudeStreamPayloadForEvent(out, "error")
	if !ok {
		t.Fatalf("missing error event payload: %q", out)
	}
	if got := payload.Get("type").String(); got != "error" {
		t.Fatalf("type = %q, want error. Payload: %s", got, payload.Raw)
	}
	if got := payload.Get("error.type").String(); got != "invalid_request_error" {
		t.Fatalf("error.type = %q, want invalid_request_error. Payload: %s", got, payload.Raw)
	}
	if got := payload.Get("error.message").String(); got != "This content was flagged for possible cybersecurity risk." {
		t.Fatalf("error.message = %q. Payload: %s", got, payload.Raw)
	}
}

func TestConvertCodexResponseToClaude_StreamErrorTypeFallbackMessage(t *testing.T) {
	ctx := context.Background()
	var param any

	outputs := ConvertCodexResponseToClaude(ctx, "", []byte(`{"messages":[]}`), nil, []byte(`data: {"type":"error","error":{},"error_type":"overloaded_error"}`), &param)
	if len(outputs) != 1 {
		t.Fatalf("expected one error chunk, got %d: %q", len(outputs), outputs)
	}

	payload, ok := firstClaudeStreamPayloadForEvent(string(outputs[0]), "error")
	if !ok {
		t.Fatalf("missing error event payload: %q", outputs[0])
	}
	if got := payload.Get("error.type").String(); got != "overloaded_error" {
		t.Fatalf("error.type = %q, want overloaded_error. Payload: %s", got, payload.Raw)
	}
	if got := payload.Get("error.message").String(); got != "overloaded_error" {
		t.Fatalf("error.message = %q, want overloaded_error. Payload: %s", got, payload.Raw)
	}
}

func TestConvertCodexResponseToClaude_StreamThinkingWithoutReasoningItemStillIncludesSignatureField(t *testing.T) {
	ctx := context.Background()
	originalRequest := []byte(`{"messages":[]}`)
	var param any

	chunks := [][]byte{
		[]byte("data: {\"type\":\"response.reasoning_summary_part.added\"}"),
		[]byte("data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"Let me think\"}"),
		[]byte("data: {\"type\":\"response.reasoning_summary_part.done\"}"),
		[]byte("data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}"),
	}

	var outputs [][]byte
	for _, chunk := range chunks {
		outputs = append(outputs, ConvertCodexResponseToClaude(ctx, "", originalRequest, nil, chunk, &param)...)
	}

	thinkingStartFound := false
	thinkingStopFound := false
	signatureDeltaFound := false

	for _, out := range outputs {
		for _, line := range strings.Split(string(out), "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := gjson.Parse(strings.TrimPrefix(line, "data: "))
			if data.Get("type").String() == "content_block_start" && data.Get("content_block.type").String() == "thinking" {
				thinkingStartFound = true
				if data.Get("content_block.signature").Exists() {
					t.Fatalf("thinking start block should NOT have signature field without encrypted_content: %s", line)
				}
			}
			if data.Get("type").String() == "content_block_stop" && data.Get("index").Int() == 0 {
				thinkingStopFound = true
			}
			if data.Get("type").String() == "content_block_delta" && data.Get("delta.type").String() == "signature_delta" {
				signatureDeltaFound = true
			}
		}
	}

	if !thinkingStartFound {
		t.Fatal("expected thinking content_block_start event")
	}
	if !thinkingStopFound {
		t.Fatal("expected thinking content_block_stop event")
	}
	if signatureDeltaFound {
		t.Fatal("did not expect signature_delta without encrypted_content")
	}
}

func TestConvertCodexResponseToClaude_StreamThinkingFinalizesPendingBlockBeforeNextSummaryPart(t *testing.T) {
	ctx := context.Background()
	originalRequest := []byte(`{"messages":[]}`)
	var param any

	chunks := [][]byte{
		[]byte("data: {\"type\":\"response.reasoning_summary_part.added\"}"),
		[]byte("data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"First part\"}"),
		[]byte("data: {\"type\":\"response.reasoning_summary_part.done\"}"),
		[]byte("data: {\"type\":\"response.reasoning_summary_part.added\"}"),
	}

	var outputs [][]byte
	for _, chunk := range chunks {
		outputs = append(outputs, ConvertCodexResponseToClaude(ctx, "", originalRequest, nil, chunk, &param)...)
	}

	startCount := 0
	stopCount := 0
	for _, out := range outputs {
		for _, line := range strings.Split(string(out), "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := gjson.Parse(strings.TrimPrefix(line, "data: "))
			if data.Get("type").String() == "content_block_start" && data.Get("content_block.type").String() == "thinking" {
				startCount++
			}
			if data.Get("type").String() == "content_block_stop" {
				stopCount++
			}
		}
	}

	if startCount != 2 {
		t.Fatalf("expected 2 thinking block starts, got %d", startCount)
	}
	if stopCount != 1 {
		t.Fatalf("expected pending thinking block to be finalized before second start, got %d stops", stopCount)
	}
}

func TestConvertCodexResponseToClaude_StreamThinkingRetainsSignatureAcrossMultipartReasoning(t *testing.T) {
	ctx := context.Background()
	originalRequest := []byte(`{"messages":[]}`)
	var param any

	chunks := [][]byte{
		[]byte("data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"reasoning\",\"encrypted_content\":\"enc_sig_multipart\"}}"),
		[]byte("data: {\"type\":\"response.reasoning_summary_part.added\"}"),
		[]byte("data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"First part\"}"),
		[]byte("data: {\"type\":\"response.reasoning_summary_part.done\"}"),
		[]byte("data: {\"type\":\"response.reasoning_summary_part.added\"}"),
		[]byte("data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"Second part\"}"),
		[]byte("data: {\"type\":\"response.reasoning_summary_part.done\"}"),
		[]byte("data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"reasoning\"}}"),
	}

	var outputs [][]byte
	for _, chunk := range chunks {
		outputs = append(outputs, ConvertCodexResponseToClaude(ctx, "", originalRequest, nil, chunk, &param)...)
	}

	signatureDeltaCount := 0
	for _, out := range outputs {
		for _, line := range strings.Split(string(out), "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := gjson.Parse(strings.TrimPrefix(line, "data: "))
			if data.Get("type").String() == "content_block_delta" && data.Get("delta.type").String() == "signature_delta" {
				signatureDeltaCount++
				if got := data.Get("delta.signature").String(); got != "enc_sig_multipart" {
					t.Fatalf("unexpected signature delta: %q", got)
				}
			}
		}
	}

	if signatureDeltaCount != 2 {
		t.Fatalf("expected signature_delta for both multipart thinking blocks, got %d", signatureDeltaCount)
	}
}

func TestConvertCodexResponseToClaude_StreamThinkingUsesEarlyCapturedSignatureWhenDoneOmitsIt(t *testing.T) {
	ctx := context.Background()
	originalRequest := []byte(`{"messages":[]}`)
	var param any

	chunks := [][]byte{
		[]byte("data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"reasoning\",\"encrypted_content\":\"enc_sig_early\"}}"),
		[]byte("data: {\"type\":\"response.reasoning_summary_part.added\"}"),
		[]byte("data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"Let me think\"}"),
		[]byte("data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"reasoning\"}}"),
	}

	var outputs [][]byte
	for _, chunk := range chunks {
		outputs = append(outputs, ConvertCodexResponseToClaude(ctx, "", originalRequest, nil, chunk, &param)...)
	}

	signatureDeltaCount := 0
	for _, out := range outputs {
		for _, line := range strings.Split(string(out), "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := gjson.Parse(strings.TrimPrefix(line, "data: "))
			if data.Get("type").String() == "content_block_delta" && data.Get("delta.type").String() == "signature_delta" {
				signatureDeltaCount++
				if got := data.Get("delta.signature").String(); got != "enc_sig_early" {
					t.Fatalf("unexpected signature delta: %q", got)
				}
			}
		}
	}

	if signatureDeltaCount != 1 {
		t.Fatalf("expected signature_delta from early-captured signature, got %d", signatureDeltaCount)
	}
}

func TestConvertCodexResponseToClaude_StreamThinkingUsesFinalDoneSignature(t *testing.T) {
	ctx := context.Background()
	originalRequest := []byte(`{"messages":[]}`)
	var param any

	chunks := [][]byte{
		[]byte("data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"reasoning\",\"encrypted_content\":\"enc_sig_initial\"}}"),
		[]byte("data: {\"type\":\"response.reasoning_summary_part.added\"}"),
		[]byte("data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"Let me think\"}"),
		[]byte("data: {\"type\":\"response.reasoning_summary_part.done\"}"),
		[]byte("data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"reasoning\",\"encrypted_content\":\"enc_sig_final\"}}"),
	}

	var outputs [][]byte
	for _, chunk := range chunks {
		outputs = append(outputs, ConvertCodexResponseToClaude(ctx, "", originalRequest, nil, chunk, &param)...)
	}

	signatureDeltaCount := 0
	events := []string{}
	for _, out := range outputs {
		for _, line := range strings.Split(string(out), "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := gjson.Parse(strings.TrimPrefix(line, "data: "))
			if data.Get("type").String() == "content_block_start" && data.Get("content_block.type").String() == "thinking" {
				events = append(events, "thinking_start")
			}
			if data.Get("type").String() == "content_block_delta" && data.Get("delta.type").String() == "thinking_delta" {
				events = append(events, "thinking_delta")
			}
			if data.Get("type").String() == "content_block_stop" && data.Get("index").Int() == 0 {
				events = append(events, "thinking_stop")
			}
			if data.Get("type").String() != "content_block_delta" || data.Get("delta.type").String() != "signature_delta" {
				continue
			}
			events = append(events, "signature_delta")
			signatureDeltaCount++
			if got := data.Get("delta.signature").String(); got != "enc_sig_final" {
				t.Fatalf("signature delta = %q, want final done signature", got)
			}
		}
	}

	if signatureDeltaCount != 1 {
		t.Fatalf("expected one signature_delta, got %d", signatureDeltaCount)
	}
	if got, want := strings.Join(events, ","), "thinking_start,thinking_delta,signature_delta,thinking_stop"; got != want {
		t.Fatalf("thinking event order = %s, want %s", got, want)
	}
}

func TestConvertCodexResponseToClaude_StreamSignatureOnlyReasoningEmitsThinkingSignature(t *testing.T) {
	ctx := context.Background()
	originalRequest := []byte(`{"messages":[]}`)
	var param any

	chunks := [][]byte{
		[]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_123\",\"model\":\"gpt-5\"}}"),
		[]byte("data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"reasoning\",\"encrypted_content\":\"enc_sig_initial\"}}"),
		[]byte("data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"reasoning\",\"encrypted_content\":\"enc_sig_only\"}}"),
		[]byte("data: {\"type\":\"response.content_part.added\"}"),
		[]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}"),
	}

	var outputs [][]byte
	for _, chunk := range chunks {
		outputs = append(outputs, ConvertCodexResponseToClaude(ctx, "", originalRequest, nil, chunk, &param)...)
	}

	thinkingStartFound := false
	thinkingDeltaFound := false
	signatureDeltaFound := false
	thinkingStopFound := false
	textStartIndex := int64(-1)
	events := []string{}

	for _, out := range outputs {
		for _, line := range strings.Split(string(out), "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := gjson.Parse(strings.TrimPrefix(line, "data: "))
			switch data.Get("type").String() {
			case "content_block_start":
				if data.Get("content_block.type").String() == "thinking" {
					events = append(events, "thinking_start")
					thinkingStartFound = true
					if got := data.Get("index").Int(); got != 0 {
						t.Fatalf("thinking block index = %d, want 0", got)
					}
				}
				if data.Get("content_block.type").String() == "text" {
					events = append(events, "text_start")
					textStartIndex = data.Get("index").Int()
				}
			case "content_block_delta":
				switch data.Get("delta.type").String() {
				case "thinking_delta":
					thinkingDeltaFound = true
				case "signature_delta":
					events = append(events, "signature_delta")
					signatureDeltaFound = true
					if got := data.Get("index").Int(); got != 0 {
						t.Fatalf("signature delta index = %d, want 0", got)
					}
					if got := data.Get("delta.signature").String(); got != "enc_sig_only" {
						t.Fatalf("unexpected signature delta: %q", got)
					}
				}
			case "content_block_stop":
				if data.Get("index").Int() == 0 {
					events = append(events, "thinking_stop")
					thinkingStopFound = true
				}
			}
		}
	}

	if !thinkingStartFound {
		t.Fatal("expected signature-only reasoning to start a thinking block")
	}
	if thinkingDeltaFound {
		t.Fatal("did not expect thinking_delta when upstream omitted summary text")
	}
	if !signatureDeltaFound {
		t.Fatal("expected signature_delta from encrypted_content-only reasoning")
	}
	if !thinkingStopFound {
		t.Fatal("expected signature-only thinking block to stop")
	}
	if textStartIndex != 1 {
		t.Fatalf("text block index = %d, want 1 after signature-only thinking block", textStartIndex)
	}
	if got, want := strings.Join(events, ","), "thinking_start,signature_delta,thinking_stop,text_start"; got != want {
		t.Fatalf("signature-only event order = %s, want %s", got, want)
	}
}

func TestConvertCodexResponseToClaudeNonStream_ThinkingIncludesSignature(t *testing.T) {
	ctx := context.Background()
	originalRequest := []byte(`{"messages":[]}`)
	response := []byte(`{
		"type":"response.completed",
		"response":{
			"id":"resp_123",
			"model":"gpt-5",
			"usage":{"input_tokens":10,"output_tokens":20},
			"output":[
				{
					"type":"reasoning",
					"encrypted_content":"enc_sig_nonstream",
					"summary":[{"type":"summary_text","text":"internal reasoning"}]
				},
				{
					"type":"message",
					"content":[{"type":"output_text","text":"final answer"}]
				}
			]
		}
	}`)

	out := ConvertCodexResponseToClaudeNonStream(ctx, "", originalRequest, nil, response, nil)
	parsed := gjson.ParseBytes(out)

	thinking := parsed.Get("content.0")
	if thinking.Get("type").String() != "thinking" {
		t.Fatalf("expected first content block to be thinking, got %s", thinking.Raw)
	}
	if got := thinking.Get("signature").String(); got != "enc_sig_nonstream" {
		t.Fatalf("expected signature to be preserved, got %q", got)
	}
	if got := thinking.Get("thinking").String(); got != "internal reasoning" {
		t.Fatalf("unexpected thinking text: %q", got)
	}
}

func TestConvertCodexResponseToClaude_StreamTextBeforeToolCallsDoesNotEmitGhostStop(t *testing.T) {
	ctx := context.Background()
	originalRequest := []byte(`{"tools":[{"name":"Read","description":"read"}]}`)
	var param any

	chunks := [][]byte{
		[]byte(`data: {"type":"response.created","response":{"id":"resp_1","model":"grok-composer-2.5-fast"}}`),
		[]byte(`data: {"type":"response.output_item.added","item":{"type":"message","status":"in_progress"},"output_index":1}`),
		[]byte(`data: {"type":"response.content_part.added","part":{"type":"output_text"},"content_index":0,"output_index":1}`),
		[]byte(`data: {"type":"response.output_text.delta","delta":"查看项目的 README 和核心入口，以便准确说明项目用途。\n","output_index":1}`),
		[]byte(`data: {"type":"response.output_item.added","item":{"type":"function_call","call_id":"call_a","name":"Read","status":"in_progress"},"output_index":2}`),
		[]byte(`data: {"type":"response.function_call_arguments.delta","delta":"{\"path\":\"/tmp/README.md\"}","output_index":2}`),
		[]byte(`data: {"type":"response.function_call_arguments.done","arguments":"{\"path\":\"/tmp/README.md\"}","output_index":2}`),
		[]byte(`data: {"type":"response.output_item.done","item":{"type":"function_call","call_id":"call_a","name":"Read","arguments":"{\"path\":\"/tmp/README.md\"}"},"output_index":2}`),
		[]byte(`data: {"type":"response.output_item.added","item":{"type":"function_call","call_id":"call_b","name":"Read","status":"in_progress"},"output_index":3}`),
		[]byte(`data: {"type":"response.function_call_arguments.delta","delta":"{\"path\":\"/tmp/main.go\"}","output_index":3}`),
		[]byte(`data: {"type":"response.content_part.done","part":{"type":"output_text"},"content_index":0,"output_index":1}`),
		[]byte(`data: {"type":"response.output_item.done","item":{"type":"message","status":"completed"},"output_index":1}`),
		[]byte(`data: {"type":"response.function_call_arguments.done","arguments":"{\"path\":\"/tmp/main.go\"}","output_index":3}`),
		[]byte(`data: {"type":"response.output_item.done","item":{"type":"function_call","call_id":"call_b","name":"Read","arguments":"{\"path\":\"/tmp/main.go\"}"},"output_index":3}`),
		[]byte(`data: {"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1}}}`),
	}

	var outputs [][]byte
	for _, chunk := range chunks {
		outputs = append(outputs, ConvertCodexResponseToClaude(ctx, "", originalRequest, nil, chunk, &param)...)
	}

	var startIndices []int64
	var stopIndices []int64
	for _, out := range outputs {
		for _, line := range strings.Split(string(out), "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := gjson.Parse(strings.TrimPrefix(line, "data: "))
			switch data.Get("type").String() {
			case "content_block_start":
				startIndices = append(startIndices, data.Get("index").Int())
			case "content_block_stop":
				stopIndices = append(stopIndices, data.Get("index").Int())
			}
		}
	}

	if len(startIndices) != 3 {
		t.Fatalf("expected 3 content_block_start events (text + 2 tools), got %v", startIndices)
	}
	if len(stopIndices) != 3 {
		t.Fatalf("expected 3 content_block_stop events, got %v", stopIndices)
	}
	if startIndices[0] != 0 || startIndices[1] != 1 || startIndices[2] != 2 {
		t.Fatalf("unexpected start indices: %v", startIndices)
	}
	if stopIndices[0] != 0 || stopIndices[1] != 1 || stopIndices[2] != 2 {
		t.Fatalf("unexpected stop indices: %v", stopIndices)
	}
}

func TestConvertCodexResponseToClaude_StreamFunctionCallDefersStartUntilDoneName(t *testing.T) {
	ctx := context.Background()
	originalRequest := []byte(`{"tools":[{"name":"web_search","description":"search"}]}`)
	var param any

	_ = ConvertCodexResponseToClaude(ctx, "", originalRequest, nil, []byte(`data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-5"}}`), &param)
	addedOutputs := ConvertCodexResponseToClaude(ctx, "", originalRequest, nil, []byte(`data: {"type":"response.output_item.added","item":{"type":"function_call","call_id":"call_1"},"output_index":1}`), &param)
	argumentsOutputs := ConvertCodexResponseToClaude(ctx, "", originalRequest, nil, []byte(`data: {"type":"response.function_call_arguments.done","arguments":"{\"query\":\"example\"}","output_index":1}`), &param)
	doneOutputs := ConvertCodexResponseToClaude(ctx, "", originalRequest, nil, []byte(`data: {"type":"response.output_item.done","item":{"type":"function_call","call_id":"call_1","name":"web_search","arguments":"{\"query\":\"example\"}"},"output_index":1}`), &param)

	if bytes.Contains(bytes.Join(addedOutputs, nil), []byte(`"content_block_start"`)) {
		t.Fatalf("function_call without name must not emit content_block_start: %q", addedOutputs)
	}
	if bytes.Contains(bytes.Join(argumentsOutputs, nil), []byte(`"input_json_delta"`)) {
		t.Fatalf("arguments must be buffered until the tool name is available: %q", argumentsOutputs)
	}

	var toolStartCount int
	var toolStopCount int
	var argumentDeltas []string
	for _, out := range doneOutputs {
		for _, line := range strings.Split(string(out), "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := gjson.Parse(strings.TrimPrefix(line, "data: "))
			switch data.Get("type").String() {
			case "content_block_start":
				if data.Get("content_block.type").String() != "tool_use" {
					continue
				}
				toolStartCount++
				if got := data.Get("content_block.name").String(); got != "web_search" {
					t.Fatalf("unexpected tool_use name %q in %s", got, data.Raw)
				}
			case "content_block_delta":
				if data.Get("delta.type").String() == "input_json_delta" {
					argumentDeltas = append(argumentDeltas, data.Get("delta.partial_json").String())
				}
			case "content_block_stop":
				toolStopCount++
			}
		}
	}

	if toolStartCount != 1 {
		t.Fatalf("expected one deferred tool_use start, got %d in %q", toolStartCount, doneOutputs)
	}
	if len(argumentDeltas) != 1 || argumentDeltas[0] != `{"query":"example"}` {
		t.Fatalf("unexpected buffered argument deltas: %v", argumentDeltas)
	}
	if toolStopCount != 1 {
		t.Fatalf("expected one deferred tool_use stop, got %d in %q", toolStopCount, doneOutputs)
	}
}

func TestConvertCodexResponseToClaude_StreamEmptyOutputUsesOutputItemDoneMessageFallback(t *testing.T) {
	ctx := context.Background()
	originalRequest := []byte(`{"tools":[]}`)
	var param any

	chunks := [][]byte{
		[]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-5\"}}"),
		[]byte("data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]},\"output_index\":0}"),
		[]byte("data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}"),
	}

	var outputs [][]byte
	for _, chunk := range chunks {
		outputs = append(outputs, ConvertCodexResponseToClaude(ctx, "", originalRequest, nil, chunk, &param)...)
	}

	foundText := false
	for _, out := range outputs {
		for _, line := range strings.Split(string(out), "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := gjson.Parse(strings.TrimPrefix(line, "data: "))
			if data.Get("type").String() == "content_block_delta" && data.Get("delta.type").String() == "text_delta" && data.Get("delta.text").String() == "ok" {
				foundText = true
				break
			}
		}
		if foundText {
			break
		}
	}
	if !foundText {
		t.Fatalf("expected fallback content from response.output_item.done message; outputs=%q", outputs)
	}
}

func TestConvertCodexResponseToClaude_StreamWebSearchCallEmitsClaudeServerToolBlocks(t *testing.T) {
	ctx := context.Background()
	originalRequest := []byte(`{
		"tools":[{"type":"web_search_20250305","name":"web_search"}],
		"messages":[{"role":"user","content":"search weather"}]
	}`)
	var param any

	chunks := [][]byte{
		[]byte(`data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-5.4"}}`),
		[]byte(`data: {"type":"response.output_item.added","item":{"id":"ws_123","type":"web_search_call","status":"in_progress"}}`),
		[]byte(`data: {"type":"response.web_search_call.searching","item_id":"ws_123"}`),
		[]byte(`data: {"type":"response.web_search_call.completed","item_id":"ws_123"}`),
		[]byte(`data: {"type":"response.output_item.done","item":{"id":"ws_123","type":"web_search_call","status":"completed","action":{"type":"search","query":"search weather"}}}`),
		[]byte(`data: {"type":"response.completed","response":{"stop_reason":"stop","usage":{"input_tokens":3,"output_tokens":2}}}`),
	}
	var outputs [][]byte
	for _, chunk := range chunks {
		outputs = append(outputs, ConvertCodexResponseToClaude(ctx, "", originalRequest, nil, chunk, &param)...)
	}
	outputText := string(bytes.Join(outputs, nil))

	for _, needle := range []string{
		`"type":"server_tool_use"`,
		`"id":"ws_123"`,
		`"type":"web_search_tool_result"`,
		`event: message_stop`,
	} {
		if !strings.Contains(outputText, needle) {
			t.Fatalf("stream output missing %s:\n%s", needle, outputText)
		}
	}
	serverToolIndex := strings.Index(outputText, `"type":"server_tool_use"`)
	resultIndex := strings.Index(outputText, `"type":"web_search_tool_result"`)
	if serverToolIndex < 0 || resultIndex < 0 || resultIndex < serverToolIndex {
		t.Fatalf("web_search_tool_result must follow server_tool_use:\n%s", outputText)
	}
	if !strings.Contains(outputText, `partial_json`) || !strings.Contains(outputText, "search weather") {
		t.Fatalf("expected web search query delta after populated output_item.done:\n%s", outputText)
	}
}

func TestConvertCodexResponseToClaude_StreamWebSearchCallReusesFallbackToolUseID(t *testing.T) {
	ctx := context.Background()
	originalRequest := []byte(`{"tools":[{"type":"web_search_20250305","name":"web_search"}],"messages":[{"role":"user","content":"search weather"}]}`)
	var param any

	chunks := [][]byte{
		[]byte(`data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-5.4"}}`),
		[]byte(`data: {"type":"response.output_item.added","item":{"type":"web_search_call","status":"in_progress"}}`),
		[]byte(`data: {"type":"response.web_search_call.completed","item_id":"ws_from_upstream"}`),
		[]byte(`data: {"type":"response.output_item.done","item":{"id":"ws_from_upstream","type":"web_search_call","status":"completed","action":{"type":"search","query":"search weather"}}}`),
		[]byte(`data: {"type":"response.completed","response":{"stop_reason":"stop","usage":{"input_tokens":3,"output_tokens":2}}}`),
	}
	var outputs [][]byte
	for _, chunk := range chunks {
		outputs = append(outputs, ConvertCodexResponseToClaude(ctx, "", originalRequest, nil, chunk, &param)...)
	}
	outputText := string(bytes.Join(outputs, nil))

	if strings.Count(outputText, `"type":"server_tool_use"`) != 1 {
		t.Fatalf("expected exactly one server_tool_use block, got output:\n%s", outputText)
	}
	if !strings.Contains(outputText, `"tool_use_id":"ws_from_upstream"`) {
		t.Fatalf("expected web_search_tool_result to reuse fallback tool_use_id:\n%s", outputText)
	}
}

func TestConvertCodexResponseToClaude_ShortensLongToolUseIDs(t *testing.T) {
	longCallID := "call_" + strings.Repeat("a", 62)
	if len(longCallID) <= 64 {
		t.Fatalf("test setup error: longCallID length = %d, want > 64", len(longCallID))
	}

	t.Run("stream", func(t *testing.T) {
		ctx := context.Background()
		originalRequest := []byte(`{"tools":[{"name":"lookup","input_schema":{"type":"object","properties":{}}}]}`)
		var param any

		outputs := ConvertCodexResponseToClaude(ctx, "", originalRequest, nil, []byte(`data: {"type":"response.output_item.added","item":{"type":"function_call","call_id":"`+longCallID+`","name":"lookup"}}`), &param)

		toolID := ""
		for _, out := range outputs {
			for _, line := range strings.Split(string(out), "\n") {
				if !strings.HasPrefix(line, "data: ") {
					continue
				}
				data := gjson.Parse(strings.TrimPrefix(line, "data: "))
				if data.Get("type").String() == "content_block_start" && data.Get("content_block.type").String() == "tool_use" {
					toolID = data.Get("content_block.id").String()
				}
			}
		}

		if toolID == "" {
			t.Fatalf("missing stream tool_use block. Outputs=%q", outputs)
		}
		if len(toolID) > 64 {
			t.Fatalf("stream tool_use id length = %d, want <= 64: %q", len(toolID), toolID)
		}
		if toolID == longCallID {
			t.Fatalf("stream tool_use id was not shortened: %q", toolID)
		}
	})

	t.Run("nonstream", func(t *testing.T) {
		ctx := context.Background()
		originalRequest := []byte(`{"tools":[{"name":"lookup","input_schema":{"type":"object","properties":{}}}]}`)
		response := []byte(`{
			"type":"response.completed",
			"response":{
				"id":"resp_1",
				"model":"gpt-5",
				"usage":{"input_tokens":1,"output_tokens":1},
				"output":[{"type":"function_call","call_id":"` + longCallID + `","name":"lookup","arguments":"{}"}]
			}
		}`)

		out := ConvertCodexResponseToClaudeNonStream(ctx, "", originalRequest, nil, response, nil)
		toolID := gjson.GetBytes(out, "content.0.id").String()
		if toolID == "" {
			t.Fatalf("missing nonstream tool_use id. Output: %s", string(out))
		}
		if len(toolID) > 64 {
			t.Fatalf("nonstream tool_use id length = %d, want <= 64: %q", len(toolID), toolID)
		}
		if toolID == longCallID {
			t.Fatalf("nonstream tool_use id was not shortened: %q", toolID)
		}
	})
}

func TestConvertCodexResponseToClaude_CachesStreamToolCallByClaudeVisibleID(t *testing.T) {
	resetCodexClaudeToolCallCacheForTest()
	t.Cleanup(resetCodexClaudeToolCallCacheForTest)

	longCallID := "call_" + strings.Repeat("b", 70)
	if len(longCallID) <= 64 {
		t.Fatalf("test setup error: longCallID length = %d, want > 64", len(longCallID))
	}

	ctx := context.Background()
	originalRequest := []byte(`{"tools":[{"name":"lookup","input_schema":{"type":"object","properties":{}}}]}`)
	var param any

	outputs := ConvertCodexResponseToClaude(ctx, "", originalRequest, nil, []byte(`data: {"type":"response.output_item.added","item":{"type":"function_call","call_id":"`+longCallID+`","name":"lookup","arguments":{"query":"repo status"}}}`), &param)

	toolID := ""
	for _, out := range outputs {
		for _, line := range strings.Split(string(out), "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := gjson.Parse(strings.TrimPrefix(line, "data: "))
			if data.Get("type").String() == "content_block_start" && data.Get("content_block.type").String() == "tool_use" {
				toolID = data.Get("content_block.id").String()
			}
		}
	}
	if toolID == "" {
		t.Fatalf("missing stream tool_use block. Outputs=%q", outputs)
	}
	if len(toolID) > 64 || toolID == longCallID {
		t.Fatalf("tool ID was not shortened for Claude visibility: %q", toolID)
	}

	request := []byte(`{
		"model":"claude-3-opus",
		"messages":[
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"` + toolID + `","content":"ok"}
			]}
		]
	}`)

	result := ConvertClaudeRequestToCodex("test-model", request, false)
	inputs := gjson.GetBytes(result, "input").Array()

	var outputIndex = -1
	for i, item := range inputs {
		if item.Get("type").String() == "function_call_output" && item.Get("call_id").String() == toolID {
			outputIndex = i
			break
		}
	}
	if outputIndex <= 0 {
		t.Fatalf("missing cached function_call before orphan tool result. Output: %s", string(result))
	}
	repairedCall := inputs[outputIndex-1]
	if got := repairedCall.Get("type").String(); got != "function_call" {
		t.Fatalf("previous input type = %q, want function_call. Output: %s", got, string(result))
	}
	if got := repairedCall.Get("call_id").String(); got != toolID {
		t.Fatalf("repaired call_id = %q, want %q. Output: %s", got, toolID, string(result))
	}
	if got := repairedCall.Get("name").String(); got != "lookup" {
		t.Fatalf("repaired tool name = %q, want lookup. Output: %s", got, string(result))
	}
	if got := repairedCall.Get("arguments.query").String(); got != "repo status" {
		t.Fatalf("repaired arguments.query = %q, want repo status. Output: %s", got, string(result))
	}
}

// TestConvertCodexResponseToClaude_GrokComposerLeadingTextThenToolsKeepsBlocksWellFormed
// reproduces the real failing grok-composer-2.5-fast stream shape captured from Claude Code
// transcripts: the model opens a leading text content part (response.content_part.added) and
// then jumps straight into function_call output items WITHOUT a response.content_part.done.
//
// Before the fix the translator reused BlockIndex 0 for the first tool_use (the text part never
// incremented it and was never closed), so the emitted Claude SSE contained two
// content_block_start events at index 0 with the text block left open. Claude Code rejects that
// stream with the synthetic "API Error: Content block not found".
func TestConvertCodexResponseToClaude_GrokComposerLeadingTextThenToolsKeepsBlocksWellFormed(t *testing.T) {
	ctx := context.Background()
	originalRequest := []byte(`{"tools":[{"name":"Read","input_schema":{"type":"object","properties":{"file_path":{"type":"string"}}}},{"name":"Grep","input_schema":{"type":"object","properties":{"pattern":{"type":"string"}}}}]}`)
	var param any

	// Faithful capture: created -> leading text part (no content_part.done) -> three function calls -> completed.
	chunks := [][]byte{
		[]byte(`data: {"type":"response.created","response":{"id":"resp_1","model":"grok-composer-2.5-fast"}}`),
		[]byte(`data: {"type":"response.content_part.added","output_index":0,"item_id":"msg_1","part":{"type":"output_text","text":""}}`),
		[]byte(`data: {"type":"response.output_item.added","output_index":1,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"Read"}}`),
		[]byte(`data: {"type":"response.function_call_arguments.delta","output_index":1,"item_id":"fc_1","delta":"{\"file_path\":\"AGENTS.md\",\"limit\":150}"}`),
		[]byte(`data: {"type":"response.output_item.done","output_index":1,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"Read","arguments":"{\"file_path\":\"AGENTS.md\",\"limit\":150}"}}`),
		[]byte(`data: {"type":"response.output_item.added","output_index":2,"item":{"id":"fc_2","type":"function_call","call_id":"call_2","name":"Read"}}`),
		[]byte(`data: {"type":"response.function_call_arguments.delta","output_index":2,"item_id":"fc_2","delta":"{\"file_path\":\"go.mod\"}"}`),
		[]byte(`data: {"type":"response.output_item.done","output_index":2,"item":{"id":"fc_2","type":"function_call","call_id":"call_2","name":"Read","arguments":"{\"file_path\":\"go.mod\"}"}}`),
		[]byte(`data: {"type":"response.output_item.added","output_index":3,"item":{"id":"fc_3","type":"function_call","call_id":"call_3","name":"Grep"}}`),
		[]byte(`data: {"type":"response.function_call_arguments.delta","output_index":3,"item_id":"fc_3","delta":"{\"pattern\":\"TODO\"}"}`),
		[]byte(`data: {"type":"response.output_item.done","output_index":3,"item":{"id":"fc_3","type":"function_call","call_id":"call_3","name":"Grep","arguments":"{\"pattern\":\"TODO\"}"}}`),
		[]byte(`data: {"type":"response.completed","response":{"id":"resp_1","stop_reason":"tool_calls","usage":{"input_tokens":1,"output_tokens":1}}}`),
	}

	var outputs [][]byte
	for _, chunk := range chunks {
		outputs = append(outputs, ConvertCodexResponseToClaude(ctx, "grok-composer-2.5-fast-high", originalRequest, nil, chunk, &param)...)
	}

	assertClaudeStreamContentBlocksWellFormed(t, outputs)

	// Three distinct tool_use blocks must be emitted, none colliding with the text block at index 0.
	var toolStartIndexes []int
	for _, ev := range parseClaudeStreamEvents(outputs) {
		if ev.Get("type").String() == "content_block_start" && ev.Get("content_block.type").String() == "tool_use" {
			toolStartIndexes = append(toolStartIndexes, int(ev.Get("index").Int()))
		}
	}
	assertIntSliceEqual(t, "tool_use start indexes", toolStartIndexes, []int{1, 2, 3}, outputs)
}

// TestConvertCodexResponseToClaude_GrokComposerDuplicateTextPartAndLateContentPartDone reproduces
// the second real failing grok-composer stream captured live (after the leading-text fix shipped):
//   - response.content_part.added is emitted twice before any text delta
//   - response.content_part.done arrives LATE, after function_call items advanced BlockIndex and a
//     tool start already closed the text block, and the final function_call has no output_item.done
//
// Before the fix the duplicate added produced two content_block_start at index 0, and the late done
// emitted a content_block_stop at the current BlockIndex (a block that was never opened) — exactly
// the "stop#3 stop#2" / "stop#6 stop#5" tails seen in the captured raw SSE. Both surface client-side
// as "API Error: Content block not found".
func TestConvertCodexResponseToClaude_GrokComposerDuplicateTextPartAndLateContentPartDone(t *testing.T) {
	ctx := context.Background()
	originalRequest := []byte(`{"tools":[{"name":"Read","input_schema":{"type":"object","properties":{"file_path":{"type":"string"}}}}]}`)
	var param any

	chunks := [][]byte{
		[]byte(`data: {"type":"response.created","response":{"id":"resp_1","model":"grok-composer-2.5-fast"}}`),
		// Duplicate content_part.added before any delta.
		[]byte(`data: {"type":"response.content_part.added","output_index":0,"item_id":"msg_1","part":{"type":"output_text","text":""}}`),
		[]byte(`data: {"type":"response.content_part.added","output_index":0,"item_id":"msg_1","part":{"type":"output_text","text":""}}`),
		[]byte(`data: {"type":"response.output_text.delta","output_index":0,"item_id":"msg_1","delta":"Executing the checklist."}`),
		// First tool: full lifecycle.
		[]byte(`data: {"type":"response.output_item.added","output_index":1,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"Read"}}`),
		[]byte(`data: {"type":"response.function_call_arguments.delta","output_index":1,"item_id":"fc_1","delta":"{\"path\":\"AGENTS.md\"}"}`),
		[]byte(`data: {"type":"response.output_item.done","output_index":1,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"Read","arguments":"{\"path\":\"AGENTS.md\"}"}}`),
		// Second tool: added + args, but NO output_item.done (closed at completion instead).
		[]byte(`data: {"type":"response.output_item.added","output_index":2,"item":{"id":"fc_2","type":"function_call","call_id":"call_2","name":"Read"}}`),
		[]byte(`data: {"type":"response.function_call_arguments.delta","output_index":2,"item_id":"fc_2","delta":"{\"path\":\"go.mod\"}"}`),
		// LATE content_part.done, after BlockIndex already advanced past the text block.
		[]byte(`data: {"type":"response.content_part.done","output_index":0,"item_id":"msg_1","part":{"type":"output_text","text":"Executing the checklist."}}`),
		[]byte(`data: {"type":"response.completed","response":{"id":"resp_1","stop_reason":"tool_calls","usage":{"input_tokens":1,"output_tokens":1}}}`),
	}

	var outputs [][]byte
	for _, chunk := range chunks {
		outputs = append(outputs, ConvertCodexResponseToClaude(ctx, "grok-composer-2.5-fast-high", originalRequest, nil, chunk, &param)...)
	}

	assertClaudeStreamContentBlocksWellFormed(t, outputs)

	// Exactly one text block (index 0) and two tool_use blocks (indexes 1, 2), each opened once.
	startsByType := map[string][]int{}
	for _, ev := range parseClaudeStreamEvents(outputs) {
		if ev.Get("type").String() == "content_block_start" {
			bt := ev.Get("content_block.type").String()
			startsByType[bt] = append(startsByType[bt], int(ev.Get("index").Int()))
		}
	}
	assertIntSliceEqual(t, "text start indexes", startsByType["text"], []int{0}, outputs)
	assertIntSliceEqual(t, "tool_use start indexes", startsByType["tool_use"], []int{1, 2}, outputs)
}

// TestConvertCodexResponseToClaude_GrokComposerThinkingThenTextKeepsBlocksWellFormed covers a
// reasoning summary that is not closed with its own *.done before the text part begins. The text
// block must not reuse the open thinking block's index.
func TestConvertCodexResponseToClaude_GrokComposerThinkingThenTextKeepsBlocksWellFormed(t *testing.T) {
	ctx := context.Background()
	originalRequest := []byte(`{"tools":[]}`)
	var param any

	chunks := [][]byte{
		[]byte(`data: {"type":"response.created","response":{"id":"resp_1","model":"grok-composer-2.5-fast"}}`),
		[]byte(`data: {"type":"response.reasoning_summary_part.added","output_index":0,"item_id":"rs_1","part":{"type":"summary_text","text":""}}`),
		[]byte(`data: {"type":"response.reasoning_summary_text.delta","output_index":0,"item_id":"rs_1","delta":"thinking..."}`),
		// No reasoning_summary_part.done: text part opens while the thinking block is still open.
		[]byte(`data: {"type":"response.content_part.added","output_index":0,"item_id":"msg_1","part":{"type":"output_text","text":""}}`),
		[]byte(`data: {"type":"response.output_text.delta","output_index":0,"item_id":"msg_1","delta":"Done."}`),
		[]byte(`data: {"type":"response.content_part.done","output_index":0,"item_id":"msg_1","part":{"type":"output_text","text":"Done."}}`),
		[]byte(`data: {"type":"response.completed","response":{"id":"resp_1","stop_reason":"stop","usage":{"input_tokens":1,"output_tokens":1}}}`),
	}

	var outputs [][]byte
	for _, chunk := range chunks {
		outputs = append(outputs, ConvertCodexResponseToClaude(ctx, "grok-composer-2.5-fast-high", originalRequest, nil, chunk, &param)...)
	}

	assertClaudeStreamContentBlocksWellFormed(t, outputs)

	startsByType := map[string][]int{}
	for _, ev := range parseClaudeStreamEvents(outputs) {
		if ev.Get("type").String() == "content_block_start" {
			bt := ev.Get("content_block.type").String()
			startsByType[bt] = append(startsByType[bt], int(ev.Get("index").Int()))
		}
	}
	assertIntSliceEqual(t, "thinking start indexes", startsByType["thinking"], []int{0}, outputs)
	assertIntSliceEqual(t, "text start indexes", startsByType["text"], []int{1}, outputs)
}

// TestConvertCodexResponseToClaude_GrokComposerDuplicateCreatedAndTrailingEvents covers stream-level
// framing weirdness: a repeated response.created must not emit a second message_start, and any event
// after response.completed (a late tool item, a duplicate completed) must be dropped rather than
// emitted after message_stop.
func TestConvertCodexResponseToClaude_GrokComposerDuplicateCreatedAndTrailingEvents(t *testing.T) {
	ctx := context.Background()
	originalRequest := []byte(`{"tools":[{"name":"Read","input_schema":{"type":"object","properties":{"file_path":{"type":"string"}}}}]}`)
	var param any

	chunks := [][]byte{
		[]byte(`data: {"type":"response.created","response":{"id":"resp_1","model":"grok-composer-2.5-fast"}}`),
		[]byte(`data: {"type":"response.created","response":{"id":"resp_1","model":"grok-composer-2.5-fast"}}`),
		[]byte(`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"Read"}}`),
		[]byte(`data: {"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc_1","delta":"{\"file_path\":\"AGENTS.md\"}"}`),
		[]byte(`data: {"type":"response.output_item.done","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"Read","arguments":"{\"file_path\":\"AGENTS.md\"}"}}`),
		[]byte(`data: {"type":"response.completed","response":{"id":"resp_1","stop_reason":"tool_calls","usage":{"input_tokens":1,"output_tokens":1}}}`),
		// Trailing events after completion must be ignored.
		[]byte(`data: {"type":"response.output_item.added","output_index":1,"item":{"id":"fc_2","type":"function_call","call_id":"call_2","name":"Read"}}`),
		[]byte(`data: {"type":"response.completed","response":{"id":"resp_1","stop_reason":"tool_calls","usage":{"input_tokens":1,"output_tokens":1}}}`),
	}

	var outputs [][]byte
	for _, chunk := range chunks {
		outputs = append(outputs, ConvertCodexResponseToClaude(ctx, "grok-composer-2.5-fast-high", originalRequest, nil, chunk, &param)...)
	}

	assertClaudeStreamContentBlocksWellFormed(t, outputs)

	starts, stops := 0, 0
	for _, ev := range parseClaudeStreamEvents(outputs) {
		switch ev.Get("type").String() {
		case "message_start":
			starts++
		case "message_stop":
			stops++
		}
	}
	if starts != 1 {
		t.Fatalf("message_start count = %d, want 1. Outputs=%q", starts, outputs)
	}
	if stops != 1 {
		t.Fatalf("message_stop count = %d, want 1. Outputs=%q", stops, outputs)
	}
}

func TestConvertCodexResponseToClaude_ParallelToolCallsKeepDistinctContentBlockIndexes(t *testing.T) {
	ctx := context.Background()
	originalRequest := []byte(`{"tools":[{"name":"Read","input_schema":{"type":"object","properties":{"file_path":{"type":"string"}}}}]}`)
	var param any

	chunks := [][]byte{
		[]byte(`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"Read"}}`),
		[]byte(`data: {"type":"response.output_item.added","output_index":1,"item":{"id":"fc_2","type":"function_call","call_id":"call_2","name":"Read"}}`),
		[]byte(`data: {"type":"response.function_call_arguments.done","output_index":0,"item_id":"fc_1","arguments":"{\"file_path\":\"AGENTS.md\"}"}`),
		[]byte(`data: {"type":"response.output_item.done","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"Read","arguments":"{\"file_path\":\"AGENTS.md\"}"}}`),
		[]byte(`data: {"type":"response.function_call_arguments.done","output_index":1,"item_id":"fc_2","arguments":"{\"file_path\":\"go.mod\"}"}`),
		[]byte(`data: {"type":"response.output_item.done","output_index":1,"item":{"id":"fc_2","type":"function_call","call_id":"call_2","name":"Read","arguments":"{\"file_path\":\"go.mod\"}"}}`),
	}

	var outputs [][]byte
	for _, chunk := range chunks {
		outputs = append(outputs, ConvertCodexResponseToClaude(ctx, "", originalRequest, nil, chunk, &param)...)
	}

	var startIndexes []int
	var stopIndexes []int
	argDeltas := map[int]string{}
	for _, out := range outputs {
		for _, line := range strings.Split(string(out), "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := gjson.Parse(strings.TrimPrefix(line, "data: "))
			switch data.Get("type").String() {
			case "content_block_start":
				if data.Get("content_block.type").String() == "tool_use" {
					startIndexes = append(startIndexes, int(data.Get("index").Int()))
				}
			case "content_block_delta":
				if data.Get("delta.type").String() == "input_json_delta" {
					if partial := data.Get("delta.partial_json").String(); partial != "" {
						argDeltas[int(data.Get("index").Int())] = partial
					}
				}
			case "content_block_stop":
				stopIndexes = append(stopIndexes, int(data.Get("index").Int()))
			}
		}
	}

	assertIntSliceEqual(t, "tool_use start indexes", startIndexes, []int{0, 1}, outputs)
	assertIntSliceEqual(t, "tool_use stop indexes", stopIndexes, []int{0, 1}, outputs)
	if got := argDeltas[0]; !strings.Contains(got, "AGENTS.md") {
		t.Fatalf("arguments for block 0 = %q, want AGENTS.md. Outputs=%q", got, outputs)
	}
	if got := argDeltas[1]; !strings.Contains(got, "go.mod") {
		t.Fatalf("arguments for block 1 = %q, want go.mod. Outputs=%q", got, outputs)
	}
}

func TestConvertCodexResponseToClaude_GrokComposerSuppressesLateToolEvents(t *testing.T) {
	ctx := context.Background()
	originalRequest := []byte(`{"tools":[{"name":"Read","input_schema":{"type":"object","properties":{"file_path":{"type":"string"}}}}]}`)
	var param any

	chunks := [][]byte{
		[]byte(`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"Read"}}`),
		[]byte(`data: {"type":"response.function_call_arguments.done","output_index":0,"item_id":"fc_1","arguments":"{\"path\":\"AGENTS.md\"}"}`),
		[]byte(`data: {"type":"response.output_item.done","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"Read","arguments":"{\"path\":\"AGENTS.md\"}"}}`),
		[]byte(`data: {"type":"response.function_call_arguments.done","output_index":0,"item_id":"fc_1","arguments":"{\"path\":\"late.md\"}"}`),
		[]byte(`data: {"type":"response.output_item.done","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"Read","arguments":"{\"path\":\"late.md\"}"}}`),
	}

	var outputs [][]byte
	for _, chunk := range chunks {
		outputs = append(outputs, ConvertCodexResponseToClaude(ctx, "grok-composer-2.5-fast-high", originalRequest, nil, chunk, &param)...)
	}

	argDeltas := 0
	stops := 0
	for _, out := range outputs {
		for _, line := range strings.Split(string(out), "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := gjson.Parse(strings.TrimPrefix(line, "data: "))
			switch data.Get("type").String() {
			case "content_block_delta":
				if data.Get("delta.type").String() == "input_json_delta" && data.Get("delta.partial_json").String() != "" {
					argDeltas++
				}
			case "content_block_stop":
				stops++
			}
		}
	}

	if argDeltas != 1 {
		t.Fatalf("non-empty argument deltas = %d, want 1. Outputs=%q", argDeltas, outputs)
	}
	if stops != 1 {
		t.Fatalf("tool block stops = %d, want 1. Outputs=%q", stops, outputs)
	}
}

func TestConvertCodexResponseToClaude_GrokComposerFinalizesOpenToolBlocksOnlyForScopedModel(t *testing.T) {
	tests := []struct {
		name      string
		model     string
		wantStops int
	}{
		{name: "grok composer fast high finalizes", model: "grok-composer-2.5-fast-high", wantStops: 1},
		{name: "grok composer fast cli suffix finalizes", model: "grok-composer-2.5-fast-high[1m]", wantStops: 1},
		{name: "other codex model unchanged", model: "gpt-5-codex", wantStops: 0},
		{name: "cursor composer unchanged", model: "composer-2.5-fast", wantStops: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			originalRequest := []byte(`{"tools":[{"name":"Read","input_schema":{"type":"object","properties":{"file_path":{"type":"string"}}}}]}`)
			var param any
			chunks := [][]byte{
				[]byte(`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"Read"}}`),
				[]byte(`data: {"type":"response.completed","response":{"stop_reason":"tool_calls","usage":{"input_tokens":1,"output_tokens":1}}}`),
			}

			var outputs [][]byte
			for _, chunk := range chunks {
				outputs = append(outputs, ConvertCodexResponseToClaude(ctx, tt.model, originalRequest, nil, chunk, &param)...)
			}

			stops := 0
			for _, out := range outputs {
				for _, line := range strings.Split(string(out), "\n") {
					if !strings.HasPrefix(line, "data: ") {
						continue
					}
					data := gjson.Parse(strings.TrimPrefix(line, "data: "))
					if data.Get("type").String() == "content_block_stop" {
						stops++
					}
				}
			}
			if stops != tt.wantStops {
				t.Fatalf("tool block stops = %d, want %d. Outputs=%q", stops, tt.wantStops, outputs)
			}
		})
	}
}

func TestConvertCodexResponseToClaude_StreamStopReasonMapping(t *testing.T) {
	tests := []struct {
		name       string
		chunks     [][]byte
		wantReason string
	}{
		{
			name: "Stop maps to end_turn",
			chunks: [][]byte{
				[]byte("data: {\"type\":\"response.completed\",\"response\":{\"stop_reason\":\"stop\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}"),
			},
			wantReason: "end_turn",
		},
		{
			name: "Incomplete max output maps to max_tokens",
			chunks: [][]byte{
				[]byte("data: {\"type\":\"response.incomplete\",\"response\":{\"incomplete_details\":{\"reason\":\"max_output_tokens\"},\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}"),
			},
			wantReason: "max_tokens",
		},
		{
			name: "Tool call wins over stop",
			chunks: [][]byte{
				[]byte("data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"lookup\"}}"),
				[]byte("data: {\"type\":\"response.completed\",\"response\":{\"stop_reason\":\"stop\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}"),
			},
			wantReason: "tool_use",
		},
		{
			name: "Content filter maps to Claude refusal",
			chunks: [][]byte{
				[]byte("data: {\"type\":\"response.incomplete\",\"response\":{\"incomplete_details\":{\"reason\":\"content_filter\"},\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}"),
			},
			wantReason: "refusal",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			originalRequest := []byte(`{"tools":[{"name":"lookup","input_schema":{"type":"object","properties":{}}}]}`)
			var param any
			var outputs [][]byte

			for _, chunk := range tt.chunks {
				outputs = append(outputs, ConvertCodexResponseToClaude(ctx, "", originalRequest, nil, chunk, &param)...)
			}

			got, ok := findClaudeStreamStopReason(outputs)
			if !ok {
				t.Fatalf("did not find message_delta stop_reason; outputs=%q", outputs)
			}
			if got != tt.wantReason {
				t.Fatalf("stop_reason = %q, want %q. Outputs=%q", got, tt.wantReason, outputs)
			}
		})
	}
}

func TestConvertCodexResponseToClaude_StreamStopSequenceMapping(t *testing.T) {
	ctx := context.Background()
	originalRequest := []byte(`{"messages":[]}`)
	var param any

	outputs := ConvertCodexResponseToClaude(ctx, "", originalRequest, nil, []byte("data: {\"type\":\"response.completed\",\"response\":{\"stop_reason\":\"stop\",\"stop_sequence\":\"\\nEND\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}"), &param)
	messageDelta, ok := findClaudeStreamMessageDelta(outputs)
	if !ok {
		t.Fatalf("did not find message_delta; outputs=%q", outputs)
	}
	if got := messageDelta.Get("delta.stop_reason").String(); got != "stop_sequence" {
		t.Fatalf("stop_reason = %q, want stop_sequence. Outputs=%q", got, outputs)
	}
	if got := messageDelta.Get("delta.stop_sequence").String(); got != "\nEND" {
		t.Fatalf("stop_sequence = %q, want newline END. Outputs=%q", got, outputs)
	}
}

func TestConvertCodexResponseToClaudeNonStream_WebSearchCallEmitsServerToolBlocks(t *testing.T) {
	ctx := context.Background()
	originalRequest := []byte(`{"tools":[{"type":"web_search_20250305","name":"web_search"}],"messages":[{"role":"user","content":"search weather"}]}`)
	response := []byte(`{"type":"response.completed","response":{"id":"resp_1","model":"gpt-5.3-codex-spark","stop_reason":"stop","usage":{"input_tokens":3,"output_tokens":2},"output":[{"type":"web_search_call","id":"ws_123","status":"completed","action":{"type":"search","query":"search weather"}},{"type":"message","content":[{"type":"output_text","text":"done"}]}]}}`)
	out := ConvertCodexResponseToClaudeNonStream(ctx, "", originalRequest, nil, response, nil)
	parsed := gjson.ParseBytes(out)
	types := []string{}
	parsed.Get("content").ForEach(func(_, value gjson.Result) bool {
		types = append(types, value.Get("type").String())
		return true
	})
	for _, want := range []string{"server_tool_use", "web_search_tool_result", "text"} {
		found := false
		for _, got := range types {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			found = strings.Contains(string(out), `"type":"`+want+`"`)
		}
		if !found {
			t.Fatalf("missing content type %s in %s", want, string(out))
		}
	}
	if parsed.Get("content.0.input.query").String() != "search weather" {
		if !strings.Contains(string(out), "search weather") {
			t.Fatalf("expected web search query in non-stream output: %s", string(out))
		}
	}
}

func TestConvertCodexResponseToClaudeNonStream_WebSearchStopReasonEndTurn(t *testing.T) {
	ctx := context.Background()
	originalRequest := []byte(`{"tools":[{"type":"web_search_20250305","name":"web_search"}],"messages":[{"role":"user","content":"search weather"}]}`)
	response := []byte(`{"type":"response.completed","response":{"id":"resp_1","model":"gpt-5.3-codex-spark","stop_reason":"stop","usage":{"input_tokens":3,"output_tokens":2},"output":[{"type":"web_search_call","id":"ws_123","status":"completed","action":{"type":"search","query":"search weather"}},{"type":"message","content":[{"type":"output_text","text":"done"}]}]}}`)
	out := ConvertCodexResponseToClaudeNonStream(ctx, "", originalRequest, nil, response, nil)
	parsed := gjson.ParseBytes(out)
	if got := parsed.Get("stop_reason").String(); got != "end_turn" {
		t.Fatalf("stop_reason = %q, want end_turn when only server web_search and text are present", got)
	}
}

func TestConvertCodexResponseToClaudeNonStream_WebSearchDedupesEmptyOpenPageItems(t *testing.T) {
	ctx := context.Background()
	originalRequest := []byte(`{"tools":[{"type":"web_search_20250305","name":"web_search"}],"messages":[{"role":"user","content":"q"}]}`)
	response := []byte(`{"type":"response.completed","response":{"id":"resp_1","model":"gpt-5.3-codex-spark","stop_reason":"stop","usage":{"input_tokens":1,"output_tokens":1},"output":[{"type":"web_search_call","id":"ws_1","status":"completed","action":{"type":"open_page"}},{"type":"web_search_call","id":"ws_1","status":"completed","action":{"type":"search","query":"weather"}},{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}}`)
	out := ConvertCodexResponseToClaudeNonStream(ctx, "", originalRequest, nil, response, nil)
	if strings.Count(string(out), `"type":"server_tool_use"`) != 1 {
		t.Fatalf("expected one server_tool_use after dedupe, got %s", string(out))
	}
	if !strings.Contains(string(out), "weather") {
		t.Fatalf("expected populated query item to be kept: %s", string(out))
	}
}

func TestConvertCodexResponseToClaudeNonStream_StopReasonMapping(t *testing.T) {
	tests := []struct {
		name       string
		response   []byte
		wantReason string
	}{
		{
			name: "Stop maps to end_turn",
			response: []byte(`{
				"type":"response.completed",
				"response":{
					"id":"resp_1",
					"model":"gpt-5",
					"stop_reason":"stop",
					"usage":{"input_tokens":1,"output_tokens":1},
					"output":[]
				}
			}`),
			wantReason: "end_turn",
		},
		{
			name: "Incomplete max output maps to max_tokens",
			response: []byte(`{
				"type":"response.incomplete",
				"response":{
					"id":"resp_1",
					"model":"gpt-5",
					"incomplete_details":{"reason":"max_output_tokens"},
					"usage":{"input_tokens":1,"output_tokens":1},
					"output":[]
				}
			}`),
			wantReason: "max_tokens",
		},
		{
			name: "Tool call wins over stop",
			response: []byte(`{
				"type":"response.completed",
				"response":{
					"id":"resp_1",
					"model":"gpt-5",
					"stop_reason":"stop",
					"usage":{"input_tokens":1,"output_tokens":1},
					"output":[{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}"}]
				}
			}`),
			wantReason: "tool_use",
		},
		{
			name: "Content filter maps to Claude refusal",
			response: []byte(`{
				"type":"response.incomplete",
				"response":{
					"id":"resp_1",
					"model":"gpt-5",
					"incomplete_details":{"reason":"content_filter"},
					"usage":{"input_tokens":1,"output_tokens":1},
					"output":[]
				}
			}`),
			wantReason: "refusal",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			originalRequest := []byte(`{"tools":[{"name":"lookup","input_schema":{"type":"object","properties":{}}}]}`)
			out := ConvertCodexResponseToClaudeNonStream(ctx, "", originalRequest, nil, tt.response, nil)
			parsed := gjson.ParseBytes(out)

			if got := parsed.Get("stop_reason").String(); got != tt.wantReason {
				t.Fatalf("stop_reason = %q, want %q. Output: %s", got, tt.wantReason, string(out))
			}
		})
	}
}

func TestConvertCodexResponseToClaudeNonStream_StopSequenceMapping(t *testing.T) {
	ctx := context.Background()
	originalRequest := []byte(`{"messages":[]}`)
	response := []byte(`{
		"type":"response.completed",
		"response":{
			"id":"resp_1",
			"model":"gpt-5",
			"stop_reason":"stop",
			"stop_sequence":"\nEND",
			"usage":{"input_tokens":1,"output_tokens":1},
			"output":[]
		}
	}`)

	out := ConvertCodexResponseToClaudeNonStream(ctx, "", originalRequest, nil, response, nil)
	parsed := gjson.ParseBytes(out)

	if got := parsed.Get("stop_reason").String(); got != "stop_sequence" {
		t.Fatalf("stop_reason = %q, want stop_sequence. Output: %s", got, string(out))
	}
	if got := parsed.Get("stop_sequence").String(); got != "\nEND" {
		t.Fatalf("stop_sequence = %q, want newline END. Output: %s", got, string(out))
	}
}

func findClaudeStreamStopReason(outputs [][]byte) (string, bool) {
	messageDelta, ok := findClaudeStreamMessageDelta(outputs)
	if !ok {
		return "", false
	}
	return messageDelta.Get("delta.stop_reason").String(), true
}

func findClaudeStreamMessageDelta(outputs [][]byte) (gjson.Result, bool) {
	for _, out := range outputs {
		for _, line := range strings.Split(string(out), "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := gjson.Parse(strings.TrimPrefix(line, "data: "))
			if data.Get("type").String() == "message_delta" {
				return data, true
			}
		}
	}
	return gjson.Result{}, false
}

func firstClaudeStreamPayloadForEvent(output, event string) (gjson.Result, bool) {
	var currentEvent string
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "event: ") {
			currentEvent = strings.TrimPrefix(line, "event: ")
			continue
		}
		if currentEvent != event || !strings.HasPrefix(line, "data: ") {
			continue
		}
		return gjson.Parse(strings.TrimPrefix(line, "data: ")), true
	}
	return gjson.Result{}, false
}

func assertIntSliceEqual(t *testing.T, label string, got, want []int, outputs [][]byte) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s = %v, want %v. Outputs=%q", label, got, want, outputs)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s = %v, want %v. Outputs=%q", label, got, want, outputs)
		}
	}
}

// parseClaudeStreamEvents flattens all SSE "data:" payloads emitted across translator calls
// into parsed gjson results, in order.
func parseClaudeStreamEvents(outputs [][]byte) []gjson.Result {
	var events []gjson.Result
	for _, out := range outputs {
		for _, line := range strings.Split(string(out), "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			events = append(events, gjson.Parse(strings.TrimPrefix(line, "data: ")))
		}
	}
	return events
}

// assertClaudeStreamContentBlocksWellFormed enforces the Claude streaming invariants that Claude
// Code relies on; a violation is exactly what surfaces client-side as "Content block not found".
//   - content_block_delta / content_block_stop must reference a currently-open block
//   - content_block_start must not reuse the index of a still-open block
//   - message_stop must come after every content block is closed, and nothing may follow it
func assertClaudeStreamContentBlocksWellFormed(t *testing.T, outputs [][]byte) {
	t.Helper()
	openBlocks := make(map[int]string)
	messageStopped := false
	for _, ev := range parseClaudeStreamEvents(outputs) {
		typ := ev.Get("type").String()
		idx := int(ev.Get("index").Int())
		switch typ {
		case "content_block_start":
			if messageStopped {
				t.Fatalf("content_block_start index=%d after message_stop. Outputs=%q", idx, outputs)
			}
			if existing, ok := openBlocks[idx]; ok {
				t.Fatalf("content_block_start index=%d reused while block still open (%s). Outputs=%q", idx, existing, outputs)
			}
			openBlocks[idx] = ev.Get("content_block.type").String()
		case "content_block_delta":
			if _, ok := openBlocks[idx]; !ok {
				t.Fatalf("content_block_delta for unopened index=%d. Outputs=%q", idx, outputs)
			}
		case "content_block_stop":
			if _, ok := openBlocks[idx]; !ok {
				t.Fatalf("content_block_stop for unopened index=%d. Outputs=%q", idx, outputs)
			}
			delete(openBlocks, idx)
		case "message_stop":
			if len(openBlocks) != 0 {
				t.Fatalf("message_stop with still-open content blocks %v. Outputs=%q", openBlocks, outputs)
			}
			messageStopped = true
		}
	}
}
