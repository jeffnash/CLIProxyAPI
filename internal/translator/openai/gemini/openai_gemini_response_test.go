package gemini

import (
	"context"
	"testing"

	"github.com/tidwall/gjson"
)

// streamConvert feeds a sequence of raw OpenAI SSE data payloads through the
// streaming converter with a single shared param (mirroring how the runtime
// drives it) and returns the concatenation of every emitted Gemini frame.
func streamConvert(t *testing.T, chunks []string) [][]byte {
	t.Helper()
	var param any
	var all [][]byte
	for _, c := range chunks {
		out := ConvertOpenAIResponseToGemini(context.Background(), "m", nil, nil, []byte(c), &param)
		all = append(all, out...)
	}
	return all
}

// findFunctionCallParts returns every functionCall object across all frames, in
// emission order, as gjson.Result values.
func findFunctionCallParts(frames [][]byte) []gjson.Result {
	var calls []gjson.Result
	for _, f := range frames {
		gjson.GetBytes(f, "candidates.0.content.parts").ForEach(func(_, part gjson.Result) bool {
			if fc := part.Get("functionCall"); fc.Exists() {
				calls = append(calls, fc)
			}
			return true
		})
	}
	return calls
}

// TestStreamEmitsFunctionCallID verifies the streaming converter writes
// functionCall.id from the accumulated OpenAI tool_call id so the Gemini client
// can echo it back as functionResponse.id (contract C-GEMID / C02).
func TestStreamEmitsFunctionCallID(t *testing.T) {
	chunks := []string{
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_abc","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"SF\"}"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	}
	frames := streamConvert(t, chunks)
	calls := findFunctionCallParts(frames)
	if len(calls) != 1 {
		t.Fatalf("expected exactly one functionCall part, got %d (frames=%v)", len(calls), frames)
	}
	if id := calls[0].Get("id").String(); id != "call_abc" {
		t.Fatalf("expected functionCall.id=call_abc, got %q", id)
	}
	if name := calls[0].Get("name").String(); name != "get_weather" {
		t.Fatalf("expected functionCall.name=get_weather, got %q", name)
	}
	if city := calls[0].Get("args.city").String(); city != "SF" {
		t.Fatalf("expected functionCall.args.city=SF, got %q", city)
	}
}

// TestStreamParallelCallsEmitIDsInToolIndexOrder verifies that two parallel
// tool calls emit BOTH ids and that part order follows ascending tool-index
// (deterministic, not Go map range). This is the parallel-desync fix in C02.
func TestStreamParallelCallsEmitIDsInToolIndexOrder(t *testing.T) {
	// Deliver the higher tool index first to prove ordering is by index, not by
	// arrival/map iteration order.
	chunks := []string{
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_second","type":"function","function":{"name":"beta","arguments":"{}"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_first","type":"function","function":{"name":"alpha","arguments":"{}"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	}
	frames := streamConvert(t, chunks)
	calls := findFunctionCallParts(frames)
	if len(calls) != 2 {
		t.Fatalf("expected two functionCall parts, got %d (frames=%v)", len(calls), frames)
	}
	// Ascending tool-index order: tool index 0 (alpha/call_first) must come first.
	if name := calls[0].Get("name").String(); name != "alpha" {
		t.Fatalf("expected first emitted part to be tool index 0 (alpha), got %q", name)
	}
	if id := calls[0].Get("id").String(); id != "call_first" {
		t.Fatalf("expected first part id=call_first, got %q", id)
	}
	if name := calls[1].Get("name").String(); name != "beta" {
		t.Fatalf("expected second emitted part to be tool index 1 (beta), got %q", name)
	}
	if id := calls[1].Get("id").String(); id != "call_second" {
		t.Fatalf("expected second part id=call_second, got %q", id)
	}
}

// TestStreamFunctionCallIDDeterministicAcrossRuns runs the same parallel-call
// stream many times and asserts the emitted (name,id) sequence is identical every
// time. This guards against the nondeterministic Go map-range ordering that C02
// replaced with sorted tool-index iteration.
func TestStreamFunctionCallIDDeterministicAcrossRuns(t *testing.T) {
	chunks := []string{
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"id0","type":"function","function":{"name":"t0","arguments":"{}"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"id1","type":"function","function":{"name":"t1","arguments":"{}"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":2,"id":"id2","type":"function","function":{"name":"t2","arguments":"{}"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":3,"id":"id3","type":"function","function":{"name":"t3","arguments":"{}"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	}
	want := []string{"t0|id0", "t1|id1", "t2|id2", "t3|id3"}
	for run := 0; run < 50; run++ {
		frames := streamConvert(t, chunks)
		calls := findFunctionCallParts(frames)
		if len(calls) != len(want) {
			t.Fatalf("run %d: expected %d parts, got %d", run, len(want), len(calls))
		}
		for i, fc := range calls {
			got := fc.Get("name").String() + "|" + fc.Get("id").String()
			if got != want[i] {
				t.Fatalf("run %d: part %d = %q, want %q (ordering not deterministic)", run, i, got, want[i])
			}
		}
	}
}

// TestStreamOmitsFunctionCallIDWhenAbsent verifies that when the OpenAI stream
// never carries an id, functionCall.id is NOT written (so the request side falls
// back to its deterministic mint instead of seeing an empty id). C-GEMID keeps
// the minted fallback for genuinely id-less calls.
func TestStreamOmitsFunctionCallIDWhenAbsent(t *testing.T) {
	chunks := []string{
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"type":"function","function":{"name":"search","arguments":"{}"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	}
	frames := streamConvert(t, chunks)
	calls := findFunctionCallParts(frames)
	if len(calls) != 1 {
		t.Fatalf("expected one functionCall part, got %d", len(calls))
	}
	if calls[0].Get("id").Exists() {
		t.Fatalf("functionCall.id must be omitted when no OpenAI id was provided, got %q", calls[0].Get("id").String())
	}
	if name := calls[0].Get("name").String(); name != "search" {
		t.Fatalf("expected functionCall.name=search, got %q", name)
	}
}

// TestNonStreamEmitsFunctionCallID verifies the non-streaming converter mirrors
// the streaming path: functionCall.id is set from the OpenAI tool_call id.
func TestNonStreamEmitsFunctionCallID(t *testing.T) {
	resp := `{
		"choices": [{
			"index": 0,
			"message": {
				"role": "assistant",
				"content": "",
				"tool_calls": [
					{"id": "call_abc", "type": "function", "function": {"name": "get_weather", "arguments": "{\"city\":\"SF\"}"}}
				]
			},
			"finish_reason": "tool_calls"
		}]
	}`
	var param any
	out := ConvertOpenAIResponseToGeminiNonStream(context.Background(), "m", nil, nil, []byte(resp), &param)
	calls := findFunctionCallParts([][]byte{out})
	if len(calls) != 1 {
		t.Fatalf("expected one functionCall part, got %d (out=%s)", len(calls), out)
	}
	if id := calls[0].Get("id").String(); id != "call_abc" {
		t.Fatalf("expected functionCall.id=call_abc, got %q", id)
	}
	if name := calls[0].Get("name").String(); name != "get_weather" {
		t.Fatalf("expected functionCall.name=get_weather, got %q", name)
	}
}

// TestNonStreamParallelCallsEmitDistinctIDs verifies two parallel tool calls in a
// non-streaming response each carry their own functionCall.id, in array order.
func TestNonStreamParallelCallsEmitDistinctIDs(t *testing.T) {
	resp := `{
		"choices": [{
			"index": 0,
			"message": {
				"role": "assistant",
				"tool_calls": [
					{"id": "call_a", "type": "function", "function": {"name": "alpha", "arguments": "{}"}},
					{"id": "call_b", "type": "function", "function": {"name": "beta", "arguments": "{}"}}
				]
			},
			"finish_reason": "tool_calls"
		}]
	}`
	var param any
	out := ConvertOpenAIResponseToGeminiNonStream(context.Background(), "m", nil, nil, []byte(resp), &param)
	calls := findFunctionCallParts([][]byte{out})
	if len(calls) != 2 {
		t.Fatalf("expected two functionCall parts, got %d (out=%s)", len(calls), out)
	}
	if calls[0].Get("id").String() != "call_a" || calls[0].Get("name").String() != "alpha" {
		t.Fatalf("expected first part alpha/call_a, got name=%q id=%q", calls[0].Get("name").String(), calls[0].Get("id").String())
	}
	if calls[1].Get("id").String() != "call_b" || calls[1].Get("name").String() != "beta" {
		t.Fatalf("expected second part beta/call_b, got name=%q id=%q", calls[1].Get("name").String(), calls[1].Get("id").String())
	}
}

// TestNonStreamOmitsFunctionCallIDWhenAbsent verifies the non-streaming converter
// does not write an empty functionCall.id when the OpenAI tool_call carries no id.
func TestNonStreamOmitsFunctionCallIDWhenAbsent(t *testing.T) {
	resp := `{
		"choices": [{
			"index": 0,
			"message": {
				"role": "assistant",
				"tool_calls": [
					{"type": "function", "function": {"name": "search", "arguments": "{}"}}
				]
			},
			"finish_reason": "tool_calls"
		}]
	}`
	var param any
	out := ConvertOpenAIResponseToGeminiNonStream(context.Background(), "m", nil, nil, []byte(resp), &param)
	calls := findFunctionCallParts([][]byte{out})
	if len(calls) != 1 {
		t.Fatalf("expected one functionCall part, got %d", len(calls))
	}
	if calls[0].Get("id").Exists() {
		t.Fatalf("functionCall.id must be omitted when no OpenAI id present, got %q", calls[0].Get("id").String())
	}
}

// --- M24 / C-KEEPALIVE no-op confirmation ---
//
// Per contract C-KEEPALIVE, the executor emits the keepalive as a raw SSE comment
// (": keepalive\n\n") DIRECTLY, bypassing this translator. The translator's only
// obligation is that a stray empty-delta chunk (if one still reaches it) renders to
// ZERO wire bytes / no malformed Gemini event. These tests pin that invariant so a
// future change cannot start emitting a malformed `data:` frame for an empty delta.

// TestStreamEmptyDeltaRendersZeroBytes verifies a content-less, tool-call-less,
// finish-reason-less delta produces no Gemini frame at all.
func TestStreamEmptyDeltaRendersZeroBytes(t *testing.T) {
	var param any
	out := ConvertOpenAIResponseToGemini(context.Background(), "m", nil, nil,
		[]byte(`data: {"choices":[{"index":0,"delta":{}}]}`), &param)
	if len(out) != 0 {
		t.Fatalf("empty delta must render to zero frames (no keepalive bytes from translator), got %d: %v", len(out), out)
	}
}

// TestStreamEmptyContentDeltaRendersZeroBytes verifies an explicitly-empty content
// string is also dropped (content == "" guard), not emitted as an empty text part.
func TestStreamEmptyContentDeltaRendersZeroBytes(t *testing.T) {
	var param any
	out := ConvertOpenAIResponseToGemini(context.Background(), "m", nil, nil,
		[]byte(`data: {"choices":[{"index":0,"delta":{"content":""}}]}`), &param)
	if len(out) != 0 {
		t.Fatalf("empty-content delta must render to zero frames, got %d: %v", len(out), out)
	}
}

// TestStreamDoneMarkerRendersZeroBytes confirms the [DONE] sentinel produces no
// frame (the executor handles end-of-stream itself).
func TestStreamDoneMarkerRendersZeroBytes(t *testing.T) {
	var param any
	out := ConvertOpenAIResponseToGemini(context.Background(), "m", nil, nil, []byte("data: [DONE]"), &param)
	if len(out) != 0 {
		t.Fatalf("[DONE] must render to zero frames, got %d: %v", len(out), out)
	}
}
