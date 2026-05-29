package responses

import (
	"context"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func parseOpenAIResponsesSSEEvent(t *testing.T, chunk []byte) (string, gjson.Result) {
	t.Helper()

	lines := strings.Split(string(chunk), "\n")
	if len(lines) < 2 {
		t.Fatalf("unexpected SSE chunk: %q", chunk)
	}

	event := strings.TrimSpace(strings.TrimPrefix(lines[0], "event:"))
	dataLine := strings.TrimSpace(strings.TrimPrefix(lines[1], "data:"))
	if !gjson.Valid(dataLine) {
		t.Fatalf("invalid SSE data JSON: %q", dataLine)
	}
	return event, gjson.Parse(dataLine)
}

func TestConvertOpenAIChatCompletionsResponseToOpenAIResponses_ResponseCompletedWaitsForDone(t *testing.T) {
	t.Parallel()

	request := []byte(`{"model":"gpt-5.4","tool_choice":"auto","parallel_tool_calls":true}`)

	tests := []struct {
		name           string
		in             []string
		doneInputIndex int // Index in tt.in where the terminal [DONE] chunk arrives and response.completed must be emitted.
		hasUsage       bool
		inputTokens    int64
		outputTokens   int64
		totalTokens    int64
	}{
		{
			// A provider may send finish_reason first and only attach usage in a later chunk (e.g. Vertex AI),
			// so response.completed must wait for [DONE] to include that usage.
			name: "late usage after finish reason",
			in: []string{
				`data: {"id":"resp_late_usage","object":"chat.completion.chunk","created":1773896263,"model":"model","choices":[{"index":0,"delta":{"role":"assistant","content":null,"reasoning_content":null,"tool_calls":[{"index":0,"id":"call_late_usage","type":"function","function":{"name":"read","arguments":""}}]},"finish_reason":null}]}`,
				`data: {"id":"resp_late_usage","object":"chat.completion.chunk","created":1773896263,"model":"model","choices":[{"index":0,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":0,"function":{"arguments":"{\"filePath\":\"C:\\\\repo\\\\README.md\"}"}}]},"finish_reason":"tool_calls"}]}`,
				`data: {"id":"resp_late_usage","object":"chat.completion.chunk","created":1773896263,"model":"model","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`,
				`data: [DONE]`,
			},
			doneInputIndex: 3,
			hasUsage:       true,
			inputTokens:    11,
			outputTokens:   7,
			totalTokens:    18,
		},
		{
			// When usage arrives on the same chunk as finish_reason, we still expect a
			// single response.completed event and it should remain deferred until [DONE].
			name: "usage on finish reason chunk",
			in: []string{
				`data: {"id":"resp_usage_same_chunk","object":"chat.completion.chunk","created":1773896263,"model":"model","choices":[{"index":0,"delta":{"role":"assistant","content":null,"reasoning_content":null,"tool_calls":[{"index":0,"id":"call_usage_same_chunk","type":"function","function":{"name":"read","arguments":""}}]},"finish_reason":null}]}`,
				`data: {"id":"resp_usage_same_chunk","object":"chat.completion.chunk","created":1773896263,"model":"model","choices":[{"index":0,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":0,"function":{"arguments":"{\"filePath\":\"C:\\\\repo\\\\README.md\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":13,"completion_tokens":5,"total_tokens":18}}`,
				`data: [DONE]`,
			},
			doneInputIndex: 2,
			hasUsage:       true,
			inputTokens:    13,
			outputTokens:   5,
			totalTokens:    18,
		},
		{
			// An OpenAI-compatible streams from a buggy server might never send usage, so response.completed should
			// still wait for [DONE] but omit the usage object entirely.
			name: "no usage chunk",
			in: []string{
				`data: {"id":"resp_no_usage","object":"chat.completion.chunk","created":1773896263,"model":"model","choices":[{"index":0,"delta":{"role":"assistant","content":null,"reasoning_content":null,"tool_calls":[{"index":0,"id":"call_no_usage","type":"function","function":{"name":"read","arguments":""}}]},"finish_reason":null}]}`,
				`data: {"id":"resp_no_usage","object":"chat.completion.chunk","created":1773896263,"model":"model","choices":[{"index":0,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":0,"function":{"arguments":"{\"filePath\":\"C:\\\\repo\\\\README.md\"}"}}]},"finish_reason":"tool_calls"}]}`,
				`data: [DONE]`,
			},
			doneInputIndex: 2,
			hasUsage:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			completedCount := 0
			completedInputIndex := -1
			var completedData gjson.Result

			// Reuse converter state across input lines to simulate one streaming response.
			var param any

			for i, line := range tt.in {
				// One upstream chunk can emit multiple downstream SSE events.
				for _, chunk := range ConvertOpenAIChatCompletionsResponseToOpenAIResponses(context.Background(), "model", request, request, []byte(line), &param) {
					event, data := parseOpenAIResponsesSSEEvent(t, chunk)
					if event != "response.completed" {
						continue
					}

					completedCount++
					completedInputIndex = i
					completedData = data
					if i < tt.doneInputIndex {
						t.Fatalf("unexpected early response.completed on input index %d", i)
					}
				}
			}

			if completedCount != 1 {
				t.Fatalf("expected exactly 1 response.completed event, got %d", completedCount)
			}
			if completedInputIndex != tt.doneInputIndex {
				t.Fatalf("expected response.completed on terminal [DONE] chunk at input index %d, got %d", tt.doneInputIndex, completedInputIndex)
			}

			// Missing upstream usage should stay omitted in the final completed event.
			if !tt.hasUsage {
				if completedData.Get("response.usage").Exists() {
					t.Fatalf("expected response.completed to omit usage when none was provided, got %s", completedData.Get("response.usage").Raw)
				}
				return
			}

			// When usage is present, the final response.completed event must preserve the usage values.
			if got := completedData.Get("response.usage.input_tokens").Int(); got != tt.inputTokens {
				t.Fatalf("unexpected response.usage.input_tokens: got %d want %d", got, tt.inputTokens)
			}
			if got := completedData.Get("response.usage.output_tokens").Int(); got != tt.outputTokens {
				t.Fatalf("unexpected response.usage.output_tokens: got %d want %d", got, tt.outputTokens)
			}
			if got := completedData.Get("response.usage.total_tokens").Int(); got != tt.totalTokens {
				t.Fatalf("unexpected response.usage.total_tokens: got %d want %d", got, tt.totalTokens)
			}
		})
	}
}

func TestConvertOpenAIChatCompletionsResponseToOpenAIResponses_MultipleToolCallsRemainSeparate(t *testing.T) {
	in := []string{
		`data: {"id":"resp_test","object":"chat.completion.chunk","created":1773896263,"model":"model","choices":[{"index":0,"delta":{"role":"assistant","content":null,"reasoning_content":null,"tool_calls":[{"index":0,"id":"call_read","type":"function","function":{"name":"read","arguments":""}}]},"finish_reason":null}]}`,
		`data: {"id":"resp_test","object":"chat.completion.chunk","created":1773896263,"model":"model","choices":[{"index":0,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":0,"function":{"arguments":"{\"filePath\":\"C:\\\\repo\",\"limit\":400,\"offset\":1}"}}]},"finish_reason":null}]}`,
		`data: {"id":"resp_test","object":"chat.completion.chunk","created":1773896263,"model":"model","choices":[{"index":0,"delta":{"role":"assistant","content":null,"reasoning_content":null,"tool_calls":[{"index":1,"id":"call_glob","type":"function","function":{"name":"glob","arguments":""}}]},"finish_reason":null}]}`,
		`data: {"id":"resp_test","object":"chat.completion.chunk","created":1773896263,"model":"model","choices":[{"index":0,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":1,"function":{"arguments":"{\"path\":\"C:\\\\repo\",\"pattern\":\"*.{yml,yaml}\"}"}}]},"finish_reason":null}]}`,
		`data: {"id":"resp_test","object":"chat.completion.chunk","created":1773896263,"model":"model","choices":[{"index":0,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":null},"finish_reason":"tool_calls"}],"usage":{"completion_tokens":10,"total_tokens":20,"prompt_tokens":10}}`,
		`data: [DONE]`,
	}

	request := []byte(`{"model":"gpt-5.4","tool_choice":"auto","parallel_tool_calls":true}`)

	var param any
	var out [][]byte
	for _, line := range in {
		out = append(out, ConvertOpenAIChatCompletionsResponseToOpenAIResponses(context.Background(), "model", request, request, []byte(line), &param)...)
	}

	addedNames := map[string]string{}
	doneArgs := map[string]string{}
	doneNames := map[string]string{}
	outputItems := map[string]gjson.Result{}

	for _, chunk := range out {
		ev, data := parseOpenAIResponsesSSEEvent(t, chunk)
		switch ev {
		case "response.output_item.added":
			if data.Get("item.type").String() != "function_call" {
				continue
			}
			addedNames[data.Get("item.call_id").String()] = data.Get("item.name").String()
		case "response.output_item.done":
			if data.Get("item.type").String() != "function_call" {
				continue
			}
			callID := data.Get("item.call_id").String()
			doneArgs[callID] = data.Get("item.arguments").String()
			doneNames[callID] = data.Get("item.name").String()
		case "response.completed":
			output := data.Get("response.output")
			for _, item := range output.Array() {
				if item.Get("type").String() == "function_call" {
					outputItems[item.Get("call_id").String()] = item
				}
			}
		}
	}

	if len(addedNames) != 2 {
		t.Fatalf("expected 2 function_call added events, got %d", len(addedNames))
	}
	if len(doneArgs) != 2 {
		t.Fatalf("expected 2 function_call done events, got %d", len(doneArgs))
	}

	if addedNames["call_read"] != "read" {
		t.Fatalf("unexpected added name for call_read: %q", addedNames["call_read"])
	}
	if addedNames["call_glob"] != "glob" {
		t.Fatalf("unexpected added name for call_glob: %q", addedNames["call_glob"])
	}

	if !gjson.Valid(doneArgs["call_read"]) {
		t.Fatalf("invalid JSON args for call_read: %q", doneArgs["call_read"])
	}
	if !gjson.Valid(doneArgs["call_glob"]) {
		t.Fatalf("invalid JSON args for call_glob: %q", doneArgs["call_glob"])
	}
	if strings.Contains(doneArgs["call_read"], "}{") {
		t.Fatalf("call_read args were concatenated: %q", doneArgs["call_read"])
	}
	if strings.Contains(doneArgs["call_glob"], "}{") {
		t.Fatalf("call_glob args were concatenated: %q", doneArgs["call_glob"])
	}

	if doneNames["call_read"] != "read" {
		t.Fatalf("unexpected done name for call_read: %q", doneNames["call_read"])
	}
	if doneNames["call_glob"] != "glob" {
		t.Fatalf("unexpected done name for call_glob: %q", doneNames["call_glob"])
	}

	if got := gjson.Get(doneArgs["call_read"], "filePath").String(); got != `C:\repo` {
		t.Fatalf("unexpected filePath for call_read: %q", got)
	}
	if got := gjson.Get(doneArgs["call_glob"], "path").String(); got != `C:\repo` {
		t.Fatalf("unexpected path for call_glob: %q", got)
	}
	if got := gjson.Get(doneArgs["call_glob"], "pattern").String(); got != "*.{yml,yaml}" {
		t.Fatalf("unexpected pattern for call_glob: %q", got)
	}

	if len(outputItems) != 2 {
		t.Fatalf("expected 2 function_call items in response.output, got %d", len(outputItems))
	}
	if outputItems["call_read"].Get("name").String() != "read" {
		t.Fatalf("unexpected response.output name for call_read: %q", outputItems["call_read"].Get("name").String())
	}
	if outputItems["call_glob"].Get("name").String() != "glob" {
		t.Fatalf("unexpected response.output name for call_glob: %q", outputItems["call_glob"].Get("name").String())
	}
}

func TestConvertOpenAIChatCompletionsResponseToOpenAIResponses_MultiChoiceToolCallsUseDistinctOutputIndexes(t *testing.T) {
	in := []string{
		`data: {"id":"resp_multi_choice","object":"chat.completion.chunk","created":1773896263,"model":"model","choices":[{"index":0,"delta":{"role":"assistant","content":null,"reasoning_content":null,"tool_calls":[{"index":0,"id":"call_choice0","type":"function","function":{"name":"glob","arguments":""}}]},"finish_reason":null},{"index":1,"delta":{"role":"assistant","content":null,"reasoning_content":null,"tool_calls":[{"index":0,"id":"call_choice1","type":"function","function":{"name":"read","arguments":""}}]},"finish_reason":null}]}`,
		`data: {"id":"resp_multi_choice","object":"chat.completion.chunk","created":1773896263,"model":"model","choices":[{"index":0,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":\"C:\\\\repo\",\"pattern\":\"*.go\"}"}}]},"finish_reason":null},{"index":1,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":0,"function":{"arguments":"{\"filePath\":\"C:\\\\repo\\\\README.md\",\"limit\":20,\"offset\":1}"}}]},"finish_reason":null}]}`,
		`data: {"id":"resp_multi_choice","object":"chat.completion.chunk","created":1773896263,"model":"model","choices":[{"index":0,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":null},"finish_reason":"tool_calls"},{"index":1,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":null},"finish_reason":"tool_calls"}],"usage":{"completion_tokens":10,"total_tokens":20,"prompt_tokens":10}}`,
		`data: [DONE]`,
	}

	request := []byte(`{"model":"gpt-5.4","tool_choice":"auto","parallel_tool_calls":true}`)

	var param any
	var out [][]byte
	for _, line := range in {
		out = append(out, ConvertOpenAIChatCompletionsResponseToOpenAIResponses(context.Background(), "model", request, request, []byte(line), &param)...)
	}

	type fcEvent struct {
		outputIndex int64
		name        string
		arguments   string
	}

	added := map[string]fcEvent{}
	done := map[string]fcEvent{}

	for _, chunk := range out {
		ev, data := parseOpenAIResponsesSSEEvent(t, chunk)
		switch ev {
		case "response.output_item.added":
			if data.Get("item.type").String() != "function_call" {
				continue
			}
			callID := data.Get("item.call_id").String()
			added[callID] = fcEvent{
				outputIndex: data.Get("output_index").Int(),
				name:        data.Get("item.name").String(),
			}
		case "response.output_item.done":
			if data.Get("item.type").String() != "function_call" {
				continue
			}
			callID := data.Get("item.call_id").String()
			done[callID] = fcEvent{
				outputIndex: data.Get("output_index").Int(),
				name:        data.Get("item.name").String(),
				arguments:   data.Get("item.arguments").String(),
			}
		}
	}

	if len(added) != 2 {
		t.Fatalf("expected 2 function_call added events, got %d", len(added))
	}
	if len(done) != 2 {
		t.Fatalf("expected 2 function_call done events, got %d", len(done))
	}

	if added["call_choice0"].name != "glob" {
		t.Fatalf("unexpected added name for call_choice0: %q", added["call_choice0"].name)
	}
	if added["call_choice1"].name != "read" {
		t.Fatalf("unexpected added name for call_choice1: %q", added["call_choice1"].name)
	}
	if added["call_choice0"].outputIndex == added["call_choice1"].outputIndex {
		t.Fatalf("expected distinct output indexes for different choices, both got %d", added["call_choice0"].outputIndex)
	}

	if !gjson.Valid(done["call_choice0"].arguments) {
		t.Fatalf("invalid JSON args for call_choice0: %q", done["call_choice0"].arguments)
	}
	if !gjson.Valid(done["call_choice1"].arguments) {
		t.Fatalf("invalid JSON args for call_choice1: %q", done["call_choice1"].arguments)
	}
	if done["call_choice0"].outputIndex == done["call_choice1"].outputIndex {
		t.Fatalf("expected distinct done output indexes for different choices, both got %d", done["call_choice0"].outputIndex)
	}
	if done["call_choice0"].name != "glob" {
		t.Fatalf("unexpected done name for call_choice0: %q", done["call_choice0"].name)
	}
	if done["call_choice1"].name != "read" {
		t.Fatalf("unexpected done name for call_choice1: %q", done["call_choice1"].name)
	}
}

func TestConvertOpenAIChatCompletionsResponseToOpenAIResponses_MixedMessageAndToolUseDistinctOutputIndexes(t *testing.T) {
	in := []string{
		`data: {"id":"resp_mixed","object":"chat.completion.chunk","created":1773896263,"model":"model","choices":[{"index":0,"delta":{"role":"assistant","content":"hello","reasoning_content":null,"tool_calls":null},"finish_reason":null},{"index":1,"delta":{"role":"assistant","content":null,"reasoning_content":null,"tool_calls":[{"index":0,"id":"call_choice1","type":"function","function":{"name":"read","arguments":""}}]},"finish_reason":null}]}`,
		`data: {"id":"resp_mixed","object":"chat.completion.chunk","created":1773896263,"model":"model","choices":[{"index":0,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":null},"finish_reason":"stop"},{"index":1,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":0,"function":{"arguments":"{\"filePath\":\"C:\\\\repo\\\\README.md\",\"limit\":20,\"offset\":1}"}}]},"finish_reason":"tool_calls"}],"usage":{"completion_tokens":10,"total_tokens":20,"prompt_tokens":10}}`,
		`data: [DONE]`,
	}

	request := []byte(`{"model":"gpt-5.4","tool_choice":"auto","parallel_tool_calls":true}`)

	var param any
	var out [][]byte
	for _, line := range in {
		out = append(out, ConvertOpenAIChatCompletionsResponseToOpenAIResponses(context.Background(), "model", request, request, []byte(line), &param)...)
	}

	var messageOutputIndex int64 = -1
	var toolOutputIndex int64 = -1

	for _, chunk := range out {
		ev, data := parseOpenAIResponsesSSEEvent(t, chunk)
		if ev != "response.output_item.added" {
			continue
		}
		switch data.Get("item.type").String() {
		case "message":
			if data.Get("item.id").String() == "msg_resp_mixed_0" {
				messageOutputIndex = data.Get("output_index").Int()
			}
		case "function_call":
			if data.Get("item.call_id").String() == "call_choice1" {
				toolOutputIndex = data.Get("output_index").Int()
			}
		}
	}

	if messageOutputIndex < 0 {
		t.Fatal("did not find message output index")
	}
	if toolOutputIndex < 0 {
		t.Fatal("did not find tool output index")
	}
	if messageOutputIndex == toolOutputIndex {
		t.Fatalf("expected distinct output indexes for message and tool call, both got %d", messageOutputIndex)
	}
}

func TestConvertOpenAIChatCompletionsResponseToOpenAIResponses_FunctionCallDoneAndCompletedOutputStayAscending(t *testing.T) {
	in := []string{
		`data: {"id":"resp_order","object":"chat.completion.chunk","created":1773896263,"model":"model","choices":[{"index":0,"delta":{"role":"assistant","content":null,"reasoning_content":null,"tool_calls":[{"index":0,"id":"call_glob","type":"function","function":{"name":"glob","arguments":""}}]},"finish_reason":null}]}`,
		`data: {"id":"resp_order","object":"chat.completion.chunk","created":1773896263,"model":"model","choices":[{"index":0,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":\"C:\\\\repo\",\"pattern\":\"*.go\"}"}}]},"finish_reason":null}]}`,
		`data: {"id":"resp_order","object":"chat.completion.chunk","created":1773896263,"model":"model","choices":[{"index":0,"delta":{"role":"assistant","content":null,"reasoning_content":null,"tool_calls":[{"index":1,"id":"call_read","type":"function","function":{"name":"read","arguments":""}}]},"finish_reason":null}]}`,
		`data: {"id":"resp_order","object":"chat.completion.chunk","created":1773896263,"model":"model","choices":[{"index":0,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":1,"function":{"arguments":"{\"filePath\":\"C:\\\\repo\\\\README.md\",\"limit\":20,\"offset\":1}"}}]},"finish_reason":null}]}`,
		`data: {"id":"resp_order","object":"chat.completion.chunk","created":1773896263,"model":"model","choices":[{"index":0,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":null},"finish_reason":"tool_calls"}],"usage":{"completion_tokens":10,"total_tokens":20,"prompt_tokens":10}}`,
		`data: [DONE]`,
	}

	request := []byte(`{"model":"gpt-5.4","tool_choice":"auto","parallel_tool_calls":true}`)

	var param any
	var out [][]byte
	for _, line := range in {
		out = append(out, ConvertOpenAIChatCompletionsResponseToOpenAIResponses(context.Background(), "model", request, request, []byte(line), &param)...)
	}

	var doneIndexes []int64
	var completedOrder []string

	for _, chunk := range out {
		ev, data := parseOpenAIResponsesSSEEvent(t, chunk)
		switch ev {
		case "response.output_item.done":
			if data.Get("item.type").String() == "function_call" {
				doneIndexes = append(doneIndexes, data.Get("output_index").Int())
			}
		case "response.completed":
			for _, item := range data.Get("response.output").Array() {
				if item.Get("type").String() == "function_call" {
					completedOrder = append(completedOrder, item.Get("call_id").String())
				}
			}
		}
	}

	if len(doneIndexes) != 2 {
		t.Fatalf("expected 2 function_call done indexes, got %d", len(doneIndexes))
	}
	if doneIndexes[0] >= doneIndexes[1] {
		t.Fatalf("expected ascending done output indexes, got %v", doneIndexes)
	}
	if len(completedOrder) != 2 {
		t.Fatalf("expected 2 function_call items in completed output, got %d", len(completedOrder))
	}
	if completedOrder[0] != "call_glob" || completedOrder[1] != "call_read" {
		t.Fatalf("unexpected completed function_call order: %v", completedOrder)
	}
}

// M24 / C-KEEPALIVE: a stray empty-content delta arriving AFTER the stream envelope is open must render to
// ZERO wire bytes (no malformed response.* event). The composer Responses keepalive is emitted as a raw
// SSE comment directly by the executor, bypassing this translator; this converter stays a no-op for the
// ping path. This test locks in that the empty delta produces nothing once started, so a future change that
// accidentally emits a synthetic (mis-sequenced) progress event here is caught.
func TestConvertOpenAIChatCompletionsResponseToOpenAIResponses_EmptyDeltaAfterStartIsZeroBytes(t *testing.T) {
	t.Parallel()

	request := []byte(`{"model":"gpt-5.4"}`)
	var param any

	// First chunk carries real content and opens the envelope.
	first := `data: {"id":"resp_ka","object":"chat.completion.chunk","created":1773896263,"model":"model","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"},"finish_reason":null}]}`
	if out := ConvertOpenAIChatCompletionsResponseToOpenAIResponses(context.Background(), "model", request, request, []byte(first), &param); len(out) == 0 {
		t.Fatalf("expected the first content chunk to open the envelope, got 0 events")
	}

	// A subsequent empty-content delta (the shape the executor would route through if it did not emit the
	// comment directly) must render to zero wire bytes.
	emptyDelta := `data: {"id":"resp_ka","object":"chat.completion.chunk","created":1773896263,"model":"model","choices":[{"index":0,"delta":{},"finish_reason":null}]}`
	out := ConvertOpenAIChatCompletionsResponseToOpenAIResponses(context.Background(), "model", request, request, []byte(emptyDelta), &param)
	if len(out) != 0 {
		t.Fatalf("expected an empty-delta chunk after start to render zero bytes, got %d events: %q", len(out), out)
	}
}

// M24 / C-KEEPALIVE: even when an empty-content delta is the FIRST chunk (e.g. the executor routes a ping
// through before any real content), the translator must open the envelope with WELL-FORMED events
// (response.created + response.in_progress) and never emit garbage. This guarantees no malformed response.*
// event can reach the client from the keepalive path.
func TestConvertOpenAIChatCompletionsResponseToOpenAIResponses_EmptyDeltaFirstChunkOpensEnvelopeCleanly(t *testing.T) {
	t.Parallel()

	request := []byte(`{"model":"gpt-5.4"}`)
	var param any

	emptyDelta := `data: {"id":"resp_ka_first","object":"chat.completion.chunk","created":1773896263,"model":"model","choices":[{"index":0,"delta":{},"finish_reason":null}]}`
	out := ConvertOpenAIChatCompletionsResponseToOpenAIResponses(context.Background(), "model", request, request, []byte(emptyDelta), &param)
	if len(out) != 2 {
		t.Fatalf("expected exactly 2 envelope events (created + in_progress), got %d: %q", len(out), out)
	}
	evCreated, dataCreated := parseOpenAIResponsesSSEEvent(t, out[0])
	if evCreated != "response.created" {
		t.Fatalf("expected first event response.created, got %q", evCreated)
	}
	if dataCreated.Get("response.id").String() != "resp_ka_first" {
		t.Fatalf("response.created carried wrong id: %q", dataCreated.Get("response.id").String())
	}
	evInProg, _ := parseOpenAIResponsesSSEEvent(t, out[1])
	if evInProg != "response.in_progress" {
		t.Fatalf("expected second event response.in_progress, got %q", evInProg)
	}

	// A second empty delta after the envelope is open is now a pure no-op.
	if more := ConvertOpenAIChatCompletionsResponseToOpenAIResponses(context.Background(), "model", request, request, []byte(emptyDelta), &param); len(more) != 0 {
		t.Fatalf("expected no further events on a second empty delta, got %d: %q", len(more), more)
	}
}

// H16 / C-RESPID: the streaming converter must surface response.id verbatim from the inbound OpenAI chunk
// id across response.created, response.in_progress and response.completed. The composer executor keys its
// outward-response-id -> sessionID map on this exact id, so any rename/synthesis here would strand
// previous_response_id follow-ups (a fresh session would be minted and context lost).
func TestConvertOpenAIChatCompletionsResponseToOpenAIResponses_ResponseIDMatchesUpstreamIDVerbatim(t *testing.T) {
	t.Parallel()

	const upstreamID = "resp_composer_fixed_id_0123456789"
	request := []byte(`{"model":"gpt-5.4"}`)
	in := []string{
		`data: {"id":"` + upstreamID + `","object":"chat.completion.chunk","created":1773896263,"model":"model","choices":[{"index":0,"delta":{"role":"assistant","content":"hello"},"finish_reason":null}]}`,
		`data: {"id":"` + upstreamID + `","object":"chat.completion.chunk","created":1773896263,"model":"model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
		`data: [DONE]`,
	}

	var param any
	seen := map[string]bool{}
	for _, line := range in {
		for _, chunk := range ConvertOpenAIChatCompletionsResponseToOpenAIResponses(context.Background(), "model", request, request, []byte(line), &param) {
			ev, data := parseOpenAIResponsesSSEEvent(t, chunk)
			switch ev {
			case "response.created", "response.in_progress", "response.completed":
				seen[ev] = true
				if got := data.Get("response.id").String(); got != upstreamID {
					t.Fatalf("%s carried response.id %q, want verbatim %q", ev, got, upstreamID)
				}
			}
		}
	}
	for _, ev := range []string{"response.created", "response.in_progress", "response.completed"} {
		if !seen[ev] {
			t.Fatalf("expected to observe %s event", ev)
		}
	}
}

// H16 / C-RESPID (non-stream): when the upstream OpenAI body carries an id, the non-stream converter must
// surface it verbatim as response.id (the composer path always supplies a fixed id and records it). The
// synthesize fallback is reserved for a buggy upstream that omitted id entirely.
func TestConvertOpenAIChatCompletionsResponseToOpenAIResponsesNonStream_ResponseIDMatchesUpstreamIDVerbatim(t *testing.T) {
	t.Parallel()

	const upstreamID = "resp_composer_nonstream_id_abcdef"
	request := []byte(`{"model":"gpt-5.4"}`)
	body := []byte(`{"id":"` + upstreamID + `","object":"chat.completion","created":1773896263,"model":"model","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`)

	out := ConvertOpenAIChatCompletionsResponseToOpenAIResponsesNonStream(context.Background(), "model", request, request, body, nil)
	if !gjson.ValidBytes(out) {
		t.Fatalf("non-stream output is not valid JSON: %q", out)
	}
	if got := gjson.GetBytes(out, "id").String(); got != upstreamID {
		t.Fatalf("non-stream response.id was %q, want verbatim %q", got, upstreamID)
	}

	// Sanity: with no upstream id, the converter synthesizes a resp_ id rather than emitting an empty one.
	bodyNoID := []byte(`{"object":"chat.completion","created":1773896263,"model":"model","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`)
	outNoID := ConvertOpenAIChatCompletionsResponseToOpenAIResponsesNonStream(context.Background(), "model", request, request, bodyNoID, nil)
	synthID := gjson.GetBytes(outNoID, "id").String()
	if synthID == "" {
		t.Fatalf("expected a synthesized id when upstream omitted id, got empty")
	}
	if !strings.HasPrefix(synthID, "resp_") {
		t.Fatalf("expected synthesized id to be prefixed resp_, got %q", synthID)
	}
}

// ADD-38 (stream false-success guard): when the upstream OpenAI chunk omits id entirely, the streaming
// converter must NOT emit an empty response.id. An empty id would round-trip as previous_response_id:"" and
// collapse the executor's composerResponseSessions key to the degenerate tenant+"\x00resp:" pre-image,
// silently routing every id-less conversation to one bridge session (a wrong-session continuation). It must
// instead synthesize a unique resp_ id (symmetric with the non-stream fallback). Composer never hits this
// branch because executeComposerStream always supplies a fixed composerResponseID(); this protects a buggy
// non-composer upstream that emitted no id.
func TestConvertOpenAIChatCompletionsResponseToOpenAIResponses_StreamMissingUpstreamIDSynthesizesNonEmpty(t *testing.T) {
	t.Parallel()

	request := []byte(`{"model":"gpt-5.4"}`)
	in := []string{
		`data: {"object":"chat.completion.chunk","created":1773896263,"model":"model","choices":[{"index":0,"delta":{"role":"assistant","content":"hello"},"finish_reason":null}]}`,
		`data: {"object":"chat.completion.chunk","created":1773896263,"model":"model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
		`data: [DONE]`,
	}

	var param any
	seenID := ""
	for _, line := range in {
		for _, chunk := range ConvertOpenAIChatCompletionsResponseToOpenAIResponses(context.Background(), "model", request, request, []byte(line), &param) {
			ev, data := parseOpenAIResponsesSSEEvent(t, chunk)
			switch ev {
			case "response.created", "response.in_progress", "response.completed":
				id := data.Get("response.id").String()
				if id == "" {
					t.Fatalf("%s emitted an EMPTY response.id (ADD-38 false-success hazard)", ev)
				}
				if !strings.HasPrefix(id, "resp_") {
					t.Fatalf("%s synthesized response.id %q, want resp_ prefix", ev, id)
				}
				if seenID == "" {
					seenID = id
				} else if id != seenID {
					// All envelope events for one streamed response must carry the SAME id, otherwise a
					// follow-up could record/look up under a different key than the client observed.
					t.Fatalf("%s carried response.id %q, inconsistent with earlier %q", ev, id, seenID)
				}
			}
		}
	}
	if seenID == "" {
		t.Fatalf("expected at least one response.* envelope event carrying a synthesized id")
	}
}

// ADD-65 / C-RESPID (continuation echo): a Responses follow-up that carries previous_response_id must have
// that value echoed back onto response.previous_response_id in BOTH the streaming response.completed event
// and the non-stream body. Clients chaining state via previous_response_id rely on this echo; dropping it
// would break the continuation contract that lets the executor resume the durable agent (the response-id ->
// sessionID lookup is keyed on the id the client passes back here as previous_response_id).
func TestConvertOpenAIChatCompletionsResponseToOpenAIResponses_PreviousResponseIDEchoedForContinuation(t *testing.T) {
	t.Parallel()

	const priorID = "resp_composer_prior_turn_0123456789"
	// The translated OpenAI request preserves previous_response_id at top level (the request translator does
	// not strip it); buildResponsesCompletedEvent / the non-stream builder read it from requestRawJSON.
	request := []byte(`{"model":"gpt-5.4","previous_response_id":"` + priorID + `"}`)

	// Streaming path: assert the echo lands on response.completed.
	in := []string{
		`data: {"id":"resp_followup_stream","object":"chat.completion.chunk","created":1773896263,"model":"model","choices":[{"index":0,"delta":{"role":"assistant","content":"continuing"},"finish_reason":null}]}`,
		`data: {"id":"resp_followup_stream","object":"chat.completion.chunk","created":1773896263,"model":"model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
		`data: [DONE]`,
	}
	var param any
	sawCompleted := false
	for _, line := range in {
		for _, chunk := range ConvertOpenAIChatCompletionsResponseToOpenAIResponses(context.Background(), "model", request, request, []byte(line), &param) {
			ev, data := parseOpenAIResponsesSSEEvent(t, chunk)
			if ev == "response.completed" {
				sawCompleted = true
				if got := data.Get("response.previous_response_id").String(); got != priorID {
					t.Fatalf("stream response.completed previous_response_id was %q, want %q", got, priorID)
				}
			}
		}
	}
	if !sawCompleted {
		t.Fatalf("expected a response.completed event in the stream")
	}

	// Non-stream path: assert the echo lands on the top-level response body.
	body := []byte(`{"id":"resp_followup_nonstream","object":"chat.completion","created":1773896263,"model":"model","choices":[{"index":0,"message":{"role":"assistant","content":"continuing"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`)
	out := ConvertOpenAIChatCompletionsResponseToOpenAIResponsesNonStream(context.Background(), "model", request, request, body, nil)
	if !gjson.ValidBytes(out) {
		t.Fatalf("non-stream output is not valid JSON: %q", out)
	}
	if got := gjson.GetBytes(out, "previous_response_id").String(); got != priorID {
		t.Fatalf("non-stream previous_response_id was %q, want %q", got, priorID)
	}
}
