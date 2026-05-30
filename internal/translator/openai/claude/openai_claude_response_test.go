package claude

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

type sseEvent struct {
	Type    string
	Payload string
}

func runStream(t *testing.T, originalReq string, chunks ...string) []sseEvent {
	t.Helper()

	var paramAny any
	var emitted [][]byte
	for _, chunk := range chunks {
		emitted = append(emitted, ConvertOpenAIResponseToClaude(
			context.Background(),
			"",
			[]byte(originalReq),
			nil,
			[]byte("data: "+chunk),
			&paramAny,
		)...)
	}
	emitted = append(emitted, ConvertOpenAIResponseToClaude(
		context.Background(),
		"",
		[]byte(originalReq),
		nil,
		[]byte("data: [DONE]"),
		&paramAny,
	)...)

	var events []sseEvent
	for _, raw := range emitted {
		s := string(raw)
		if !strings.HasPrefix(s, "event: ") {
			continue
		}
		nl := strings.Index(s, "\n")
		if nl < 0 {
			continue
		}
		typ := strings.TrimPrefix(s[:nl], "event: ")
		rest := s[nl+1:]
		if !strings.HasPrefix(rest, "data: ") {
			continue
		}
		payload := strings.TrimRight(strings.TrimPrefix(rest, "data: "), "\n")
		events = append(events, sseEvent{Type: typ, Payload: payload})
	}
	return events
}

func countByType(events []sseEvent, typ string) int {
	n := 0
	for _, e := range events {
		if e.Type == typ {
			n++
		}
	}
	return n
}

func toolUseStarts(events []sseEvent) []sseEvent {
	var out []sseEvent
	for _, e := range events {
		if e.Type != "content_block_start" {
			continue
		}
		if gjson.Get(e.Payload, "content_block.type").String() == "tool_use" {
			out = append(out, e)
		}
	}
	return out
}

func blockIndices(events []sseEvent) []int64 {
	var idx []int64
	for _, e := range events {
		if e.Type == "content_block_start" {
			idx = append(idx, gjson.Get(e.Payload, "index").Int())
		}
	}
	return idx
}

func lastStopReason(events []sseEvent) string {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == "message_delta" {
			return gjson.Get(events[i].Payload, "delta.stop_reason").String()
		}
	}
	return ""
}

const streamReq = `{"stream":true}`

func TestConvertOpenAIResponseToClaude_StreamIgnoresNullToolNameDelta(t *testing.T) {
	originalRequest := []byte(streamReq)
	var param any

	firstChunks := ConvertOpenAIResponseToClaude(
		context.Background(),
		"test-model",
		originalRequest,
		nil,
		[]byte(`data: {"id":"chatcmpl_1","model":"test-model","created":1,"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read_file","arguments":""}}]},"finish_reason":null}]}`),
		&param,
	)
	firstOutput := bytes.Join(firstChunks, nil)
	if !bytes.Contains(firstOutput, []byte(`"name":"read_file"`)) {
		t.Fatalf("expected first chunk to start read_file tool block, got %s", string(firstOutput))
	}

	secondChunks := ConvertOpenAIResponseToClaude(
		context.Background(),
		"test-model",
		originalRequest,
		nil,
		[]byte(`data: {"id":"chatcmpl_1","model":"test-model","created":1,"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":null,"arguments":"{\"path\":\"/tmp/a\"}"}}]},"finish_reason":null}]}`),
		&param,
	)
	secondOutput := bytes.Join(secondChunks, nil)
	if bytes.Contains(secondOutput, []byte(`content_block_start`)) {
		t.Fatalf("did not expect null tool name delta to start a new content block, got %s", string(secondOutput))
	}
	if bytes.Contains(secondOutput, []byte(`"name":""`)) {
		t.Fatalf("did not expect null tool name delta to emit an empty tool name, got %s", string(secondOutput))
	}
}

func TestStreamingTool_EmptyNameThroughout(t *testing.T) {
	events := runStream(t, streamReq,
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_a","function":{"name":"","arguments":""}}]}}]}`,
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"","arguments":"{\"x\":1}"}}]}}]}`,
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	)

	if got := len(toolUseStarts(events)); got != 0 {
		t.Fatalf("expected zero tool_use content_block_start, got %d (events=%+v)", got, events)
	}
	if got := countByType(events, "content_block_delta"); got != 0 {
		t.Fatalf("expected zero content_block_delta when start was suppressed, got %d", got)
	}
	if got := countByType(events, "content_block_stop"); got != 0 {
		t.Fatalf("expected zero content_block_stop when start was suppressed, got %d", got)
	}
	if got := lastStopReason(events); got == "tool_use" {
		t.Fatalf("stop_reason must not be tool_use when zero tool_use blocks were emitted; got %q", got)
	}
}

func TestStreamingTool_NullName(t *testing.T) {
	events := runStream(t, streamReq,
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_a","function":{"name":null,"arguments":""}}]}}]}`,
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	)
	if got := len(toolUseStarts(events)); got != 0 {
		t.Fatalf("null name must not produce a tool_use start; got %d", got)
	}
	if got := countByType(events, "content_block_stop"); got != 0 {
		t.Fatalf("null name must not produce content_block_stop; got %d", got)
	}
}

func TestStreamingTool_NonStringName(t *testing.T) {
	events := runStream(t, streamReq,
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_a","function":{"name":123,"arguments":""}}]}}]}`,
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	)
	if got := len(toolUseStarts(events)); got != 0 {
		t.Fatalf("non-string name must not produce a tool_use start; got %d", got)
	}
}

func TestStreamingTool_RepeatedName(t *testing.T) {
	events := runStream(t, streamReq,
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_a","function":{"name":"do_it","arguments":""}}]}}]}`,
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"do_it","arguments":"{\"x\""}}]}}]}`,
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"do_it","arguments":":1}"}}]}}]}`,
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	)

	starts := toolUseStarts(events)
	if len(starts) != 1 {
		t.Fatalf("expected exactly one tool_use start, got %d", len(starts))
	}
	if name := gjson.Get(starts[0].Payload, "content_block.name").String(); name != "do_it" {
		t.Fatalf("announced tool name = %q, want %q", name, "do_it")
	}
	if got := countByType(events, "content_block_stop"); got != 1 {
		t.Fatalf("expected exactly one content_block_stop, got %d", got)
	}
}

func TestStreamingTool_MixedSuppressedAndValid(t *testing.T) {
	events := runStream(t, streamReq,
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[
			{"index":0,"id":"call_skip","function":{"name":"","arguments":""}},
			{"index":1,"id":"call_real","function":{"name":"do_it","arguments":""}}
		]}}]}`,
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[
			{"index":1,"function":{"arguments":"{}"}}
		]}}]}`,
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	)

	starts := toolUseStarts(events)
	if len(starts) != 1 {
		t.Fatalf("expected exactly one tool_use start, got %d", len(starts))
	}
	if got := countByType(events, "content_block_stop"); got != 1 {
		t.Fatalf("expected exactly one content_block_stop, got %d", got)
	}

	indices := blockIndices(events)
	if len(indices) == 0 || indices[0] != 0 {
		t.Fatalf("first content_block_start index must be 0, got %v", indices)
	}
}

func TestStreamingTool_EmptyIDDeferStart(t *testing.T) {
	events := runStream(t, streamReq,
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"","function":{"name":"do_it","arguments":""}}]}}]}`,
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_real","function":{"arguments":"{}"}}]}}]}`,
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	)

	starts := toolUseStarts(events)
	if len(starts) != 1 {
		t.Fatalf("expected exactly one tool_use start once id arrived, got %d", len(starts))
	}
	if id := gjson.Get(starts[0].Payload, "content_block.id").String(); id != "call_real" {
		t.Fatalf("announced tool id = %q, want %q", id, "call_real")
	}
}

func TestStreamingTool_IDInDeltaWithoutFunction(t *testing.T) {
	events := runStream(t, streamReq,
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"function":{"name":"do_it"}}]}}]}`,
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_real"}]}}]}`,
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{}"}}]}}]}`,
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	)

	starts := toolUseStarts(events)
	if len(starts) != 1 {
		t.Fatalf("expected exactly one tool_use start when id arrives in a function-less delta, got %d", len(starts))
	}
	if id := gjson.Get(starts[0].Payload, "content_block.id").String(); id != "call_real" {
		t.Fatalf("announced tool id = %q, want %q", id, "call_real")
	}
	if name := gjson.Get(starts[0].Payload, "content_block.name").String(); name != "do_it" {
		t.Fatalf("announced tool name = %q, want %q", name, "do_it")
	}
	if got := countByType(events, "content_block_stop"); got != 1 {
		t.Fatalf("expected exactly one content_block_stop, got %d", got)
	}
}

func TestStreamingTool_StopReasonWithEmittedTool(t *testing.T) {
	events := runStream(t, streamReq,
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_a","function":{"name":"do_it","arguments":"{}"}}]}}]}`,
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`,
	)
	if got := lastStopReason(events); got != "tool_use" {
		t.Fatalf("stop_reason = %q, want %q", got, "tool_use")
	}
}

func TestStreamingTool_StopReasonWhenIDNeverArrives(t *testing.T) {
	events := runStream(t, streamReq,
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"function":{"name":"do_it","arguments":""}}]}}]}`,
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{}"}}]}}]}`,
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	)

	starts := toolUseStarts(events)
	if len(starts) != 1 {
		t.Fatalf("expected one belated tool_use start with synthetic id, got %d", len(starts))
	}
	id := gjson.Get(starts[0].Payload, "content_block.id").String()
	if !strings.HasPrefix(id, "toolu_") {
		t.Fatalf("synthetic id should match toolu_<nanos>_<n>, got %q", id)
	}
	if name := gjson.Get(starts[0].Payload, "content_block.name").String(); name != "do_it" {
		t.Fatalf("announced tool name = %q, want %q", name, "do_it")
	}
	if got := lastStopReason(events); got != "tool_use" {
		t.Fatalf("stop_reason = %q, want %q", got, "tool_use")
	}
}

func TestStreamingTool_BelatedStartsUseOpenAIToolIndexOrder(t *testing.T) {
	events := runStream(t, streamReq,
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[
			{"index":2,"function":{"name":"third_tool","arguments":"{}"}},
			{"index":0,"function":{"name":"first_tool","arguments":"{}"}},
			{"index":1,"function":{"name":"second_tool","arguments":"{}"}}
		]}}]}`,
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	)

	starts := toolUseStarts(events)
	if len(starts) != 3 {
		t.Fatalf("expected three belated tool_use starts, got %d", len(starts))
	}

	wantNames := []string{"first_tool", "second_tool", "third_tool"}
	for i, wantName := range wantNames {
		if name := gjson.Get(starts[i].Payload, "content_block.name").String(); name != wantName {
			t.Fatalf("tool_use start %d name = %q, want %q (starts=%+v)", i, name, wantName, starts)
		}
		if blockIndex := gjson.Get(starts[i].Payload, "index").Int(); blockIndex != int64(i) {
			t.Fatalf("tool_use start %d block index = %d, want %d", i, blockIndex, i)
		}
	}
}

func TestStreamingTool_LateIDAfterFinalization(t *testing.T) {
	events := runStream(t, streamReq,
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"function":{"name":"do_it"}}]}}]}`,
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`,
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_late"}]}}]}`,
	)

	starts := toolUseStarts(events)
	if len(starts) != 1 {
		t.Fatalf("expected one belated tool_use start, got %d", len(starts))
	}

	var sawMessageStop bool
	for _, e := range events {
		if e.Type == "message_stop" {
			sawMessageStop = true
			continue
		}
		if sawMessageStop {
			switch e.Type {
			case "content_block_start", "content_block_delta", "content_block_stop":
				t.Fatalf("event %q emitted after message_stop (events=%+v)", e.Type, events)
			}
		}
	}
}

func TestStreamingTool_StopReasonMixedSuppressedAndValid(t *testing.T) {
	events := runStream(t, streamReq,
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[
			{"index":0,"id":"call_skip","function":{"name":"","arguments":""}},
			{"index":1,"id":"call_real","function":{"name":"do_it","arguments":"{}"}}
		]}}]}`,
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	)
	if got := lastStopReason(events); got != "tool_use" {
		t.Fatalf("stop_reason = %q, want %q", got, "tool_use")
	}
}

// ---- ADD-66: thinking emission gated on the inbound request enabling thinking ----

// thinkingStartCount counts content_block_start events of type "thinking" in a
// stream event slice.
func thinkingStartCount(events []sseEvent) int {
	n := 0
	for _, e := range events {
		if e.Type == "content_block_start" && gjson.Get(e.Payload, "content_block.type").String() == "thinking" {
			n++
		}
	}
	return n
}

// thinkingDeltaCount counts content_block_delta events carrying a thinking_delta.
func thinkingDeltaCount(events []sseEvent) int {
	n := 0
	for _, e := range events {
		if e.Type == "content_block_delta" && gjson.Get(e.Payload, "delta.type").String() == "thinking_delta" {
			n++
		}
	}
	return n
}

func TestThinkingEnabledForClaude(t *testing.T) {
	cases := []struct {
		name       string
		original   string
		translated string
		want       bool
	}{
		{"no thinking field", `{"stream":true}`, "", false},
		{"thinking enabled", `{"thinking":{"type":"enabled","budget_tokens":1024}}`, "", true},
		{"thinking adaptive", `{"thinking":{"type":"adaptive"}}`, "", true},
		{"thinking auto", `{"thinking":{"type":"auto"}}`, "", true},
		{"thinking disabled", `{"thinking":{"type":"disabled"}}`, `{"reasoning_effort":"none"}`, false},
		{"thinking disabled (no fallback)", `{"thinking":{"type":"disabled"}}`, "", false},
		{"fallback reasoning_effort high", `{"stream":true}`, `{"reasoning_effort":"high"}`, true},
		{"fallback reasoning_effort none", `{"stream":true}`, `{"reasoning_effort":"none"}`, false},
		{"fallback reasoning_effort empty", `{"stream":true}`, `{"reasoning_effort":""}`, false},
		{"upper-case enabled", `{"thinking":{"type":"ENABLED"}}`, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := thinkingEnabledForClaude([]byte(tc.original), []byte(tc.translated))
			if got != tc.want {
				t.Fatalf("thinkingEnabledForClaude(%q,%q) = %v, want %v", tc.original, tc.translated, got, tc.want)
			}
		})
	}
}

// Stream: when the request did NOT enable thinking, reasoning_content deltas
// must NOT produce any Anthropic thinking blocks (text still flows).
func TestStream_ThinkingNotRequested_DropsReasoning(t *testing.T) {
	events := runStream(t, `{"stream":true}`,
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"secret chain of thought"}}]}`,
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"content":"hello"}}]}`,
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	)
	if got := thinkingStartCount(events); got != 0 {
		t.Fatalf("expected zero thinking content_block_start, got %d (events=%+v)", got, events)
	}
	if got := thinkingDeltaCount(events); got != 0 {
		t.Fatalf("expected zero thinking_delta, got %d (events=%+v)", got, events)
	}
	// The visible text must still be delivered.
	var sawText bool
	for _, e := range events {
		if e.Type == "content_block_delta" && gjson.Get(e.Payload, "delta.type").String() == "text_delta" {
			sawText = true
		}
	}
	if !sawText {
		t.Fatalf("expected text_delta to still flow when thinking suppressed, events=%+v", events)
	}
}

// Stream: when thinking IS enabled, reasoning_content deltas produce thinking blocks.
func TestStream_ThinkingRequested_EmitsReasoning(t *testing.T) {
	events := runStream(t, `{"stream":true,"thinking":{"type":"enabled","budget_tokens":2048}}`,
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"thinking step"}}]}`,
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"content":"hello"}}]}`,
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	)
	if got := thinkingStartCount(events); got != 1 {
		t.Fatalf("expected one thinking content_block_start, got %d (events=%+v)", got, events)
	}
	if got := thinkingDeltaCount(events); got != 1 {
		t.Fatalf("expected one thinking_delta, got %d (events=%+v)", got, events)
	}
}

// Nonstream (chat-choice path, stream:false): thinking gating.
func TestNonStreamChoice_ThinkingGating(t *testing.T) {
	chunk := `{"id":"c1","model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","reasoning_content":"hidden","content":"hi"}}]}`

	// Not requested -> no thinking block.
	var p1 any
	out1 := ConvertOpenAIResponseToClaude(context.Background(), "", []byte(`{"stream":false}`), nil, []byte("data: "+chunk), &p1)
	joined1 := joinBytes(out1)
	if bytes.Contains(joined1, []byte(`"type":"thinking"`)) {
		t.Fatalf("expected no thinking block when thinking not requested, got %s", joined1)
	}
	if !bytes.Contains(joined1, []byte(`"type":"text"`)) {
		t.Fatalf("expected text block to remain, got %s", joined1)
	}

	// Requested -> thinking block present.
	var p2 any
	out2 := ConvertOpenAIResponseToClaude(context.Background(), "", []byte(`{"stream":false,"thinking":{"type":"enabled","budget_tokens":1024}}`), nil, []byte("data: "+chunk), &p2)
	joined2 := joinBytes(out2)
	if !bytes.Contains(joined2, []byte(`"type":"thinking"`)) {
		t.Fatalf("expected thinking block when thinking requested, got %s", joined2)
	}
}

// ConvertOpenAIResponseToClaudeNonStream: both the message.reasoning_content
// path and the array-form "reasoning" content path must be gated.
func TestNonStream_ReasoningContent_Gated(t *testing.T) {
	body := `{"id":"c1","model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","reasoning_content":"hidden","content":"hi"}}]}`

	off := ConvertOpenAIResponseToClaudeNonStream(context.Background(), "", []byte(`{"stream":false}`), nil, []byte(body), nil)
	if bytes.Contains(off, []byte(`"type":"thinking"`)) {
		t.Fatalf("expected no thinking block (reasoning_content) when not requested, got %s", off)
	}
	if !bytes.Contains(off, []byte(`"type":"text"`)) {
		t.Fatalf("expected text block to remain, got %s", off)
	}

	on := ConvertOpenAIResponseToClaudeNonStream(context.Background(), "", []byte(`{"stream":false,"thinking":{"type":"enabled","budget_tokens":1024}}`), nil, []byte(body), nil)
	if !bytes.Contains(on, []byte(`"type":"thinking"`)) {
		t.Fatalf("expected thinking block (reasoning_content) when requested, got %s", on)
	}
}

func TestNonStream_ArrayReasoning_Gated(t *testing.T) {
	// content as an array containing a "reasoning" item.
	body := `{"id":"c1","model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":[{"type":"reasoning","text":"hidden cot"},{"type":"text","text":"hi"}]}}]}`

	off := ConvertOpenAIResponseToClaudeNonStream(context.Background(), "", []byte(`{"stream":false}`), nil, []byte(body), nil)
	if bytes.Contains(off, []byte(`"type":"thinking"`)) {
		t.Fatalf("expected no thinking block (array reasoning) when not requested, got %s", off)
	}
	if !bytes.Contains(off, []byte(`"type":"text"`)) {
		t.Fatalf("expected text block to remain, got %s", off)
	}

	on := ConvertOpenAIResponseToClaudeNonStream(context.Background(), "", []byte(`{"stream":false,"thinking":{"type":"adaptive"}}`), nil, []byte(body), nil)
	if !bytes.Contains(on, []byte(`"type":"thinking"`)) {
		t.Fatalf("expected thinking block (array reasoning) when requested, got %s", on)
	}
}

// Fallback signal: a translated request that carries a non-"none"
// reasoning_effort (but no thinking field on the original) still enables thinking.
func TestNonStream_ReasoningEffortFallback_Enables(t *testing.T) {
	body := `{"id":"c1","model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","reasoning_content":"hidden","content":"hi"}}]}`
	out := ConvertOpenAIResponseToClaudeNonStream(context.Background(), "", []byte(`{"stream":false}`), []byte(`{"reasoning_effort":"high"}`), []byte(body), nil)
	if !bytes.Contains(out, []byte(`"type":"thinking"`)) {
		t.Fatalf("expected thinking block via reasoning_effort fallback, got %s", out)
	}
}

func joinBytes(chunks [][]byte) []byte {
	var b bytes.Buffer
	for _, c := range chunks {
		b.Write(c)
	}
	return b.Bytes()
}
