package gemini

import (
	"testing"

	"github.com/tidwall/gjson"
)

// collectToolCallIDs returns, in order, the ids of every assistant tool_call and
// the tool_call_id of every role:tool message in the converted OpenAI request.
func collectToolCallIDs(t *testing.T, out []byte) (callIDs []string, respIDs []string) {
	t.Helper()
	gjson.GetBytes(out, "messages").ForEach(func(_, msg gjson.Result) bool {
		switch msg.Get("role").String() {
		case "assistant":
			msg.Get("tool_calls").ForEach(func(_, tc gjson.Result) bool {
				callIDs = append(callIDs, tc.Get("id").String())
				return true
			})
		case "tool":
			respIDs = append(respIDs, msg.Get("tool_call_id").String())
		}
		return true
	})
	return callIDs, respIDs
}

// TestGeminiFunctionCallIDRoundTripsWhenClientSendsID verifies that when a Gemini
// client preserves the functionCall.id / functionResponse.id, those exact ids are
// used as the OpenAI tool_call_id so they round-trip (the bridge sees the id it
// emitted). This is the core of the G1 CRITICAL fix.
func TestGeminiFunctionCallIDRoundTripsWhenClientSendsID(t *testing.T) {
	req := `{
		"contents": [
			{"role": "model", "parts": [
				{"functionCall": {"id": "call_abc123", "name": "get_weather", "args": {"city": "SF"}}}
			]},
			{"role": "user", "parts": [
				{"functionResponse": {"id": "call_abc123", "name": "get_weather", "response": {"content": "sunny"}}}
			]}
		]
	}`

	out := ConvertGeminiRequestToOpenAI("m", []byte(req), false)

	callIDs, respIDs := collectToolCallIDs(t, out)
	if len(callIDs) != 1 || callIDs[0] != "call_abc123" {
		t.Fatalf("expected tool_call id call_abc123, got %v", callIDs)
	}
	if len(respIDs) != 1 || respIDs[0] != "call_abc123" {
		t.Fatalf("expected response tool_call_id call_abc123 (round-trip), got %v", respIDs)
	}
}

// TestGeminiFunctionCallIDRoundTripsAcrossTurns is the actual composer-continuation
// case that always 500'd: the functionCall and functionResponse live in DIFFERENT
// contents entries (separate turns) yet carry the same client id. The id must still
// round-trip, since the bridge keys continuations by tool_call_id.
func TestGeminiFunctionCallIDRoundTripsAcrossTurns(t *testing.T) {
	req := `{
		"contents": [
			{"role": "user", "parts": [{"text": "what's the weather?"}]},
			{"role": "model", "parts": [
				{"functionCall": {"id": "toolu_xyz", "name": "get_weather", "args": {}}}
			]},
			{"role": "user", "parts": [
				{"functionResponse": {"id": "toolu_xyz", "name": "get_weather", "response": {"content": "rainy"}}},
				{"text": "thanks, now compare with yesterday"}
			]}
		]
	}`

	out := ConvertGeminiRequestToOpenAI("m", []byte(req), false)

	callIDs, respIDs := collectToolCallIDs(t, out)
	if len(callIDs) != 1 || callIDs[0] != "toolu_xyz" {
		t.Fatalf("expected tool_call id toolu_xyz, got %v", callIDs)
	}
	if len(respIDs) != 1 || respIDs[0] != "toolu_xyz" {
		t.Fatalf("expected response tool_call_id toolu_xyz to round-trip across turns, got %v", respIDs)
	}
}

// TestGeminiFunctionCallIDMintedDeterministicallyWhenAbsent verifies that when the
// client sends NO id, a call and its response get the SAME deterministic id so they
// still pair up (the no-id mint path of G1).
func TestGeminiFunctionCallIDMintedDeterministicallyWhenAbsent(t *testing.T) {
	req := `{
		"contents": [
			{"role": "model", "parts": [
				{"functionCall": {"name": "search", "args": {"q": "go"}}}
			]},
			{"role": "user", "parts": [
				{"functionResponse": {"name": "search", "response": {"content": "results"}}}
			]}
		]
	}`

	out := ConvertGeminiRequestToOpenAI("m", []byte(req), false)

	callIDs, respIDs := collectToolCallIDs(t, out)
	if len(callIDs) != 1 || len(respIDs) != 1 {
		t.Fatalf("expected exactly one call and one response, got calls=%v resps=%v", callIDs, respIDs)
	}
	if callIDs[0] == "" {
		t.Fatalf("expected a minted (non-empty) tool_call id, got empty")
	}
	if callIDs[0] != respIDs[0] {
		t.Fatalf("call and response must share the same deterministic id; call=%q resp=%q", callIDs[0], respIDs[0])
	}
	// Deterministic shape: derived from the function name + ordinal, never random.
	if callIDs[0] != "call_search_0" {
		t.Fatalf("expected deterministic id call_search_0, got %q", callIDs[0])
	}
}

// TestGeminiDistinctCallsGetDistinctIDs verifies two distinct id-less calls (same
// function name) get DISTINCT deterministic ids, and each pairs with its own response
// in order — the interleaving that the old toolCallIDs[len-1] heuristic collapsed.
func TestGeminiDistinctCallsGetDistinctIDs(t *testing.T) {
	req := `{
		"contents": [
			{"role": "model", "parts": [
				{"functionCall": {"name": "read_file", "args": {"path": "a"}}},
				{"functionCall": {"name": "read_file", "args": {"path": "b"}}}
			]},
			{"role": "user", "parts": [
				{"functionResponse": {"name": "read_file", "response": {"content": "A"}}},
				{"functionResponse": {"name": "read_file", "response": {"content": "B"}}}
			]}
		]
	}`

	out := ConvertGeminiRequestToOpenAI("m", []byte(req), false)

	callIDs, respIDs := collectToolCallIDs(t, out)
	if len(callIDs) != 2 || len(respIDs) != 2 {
		t.Fatalf("expected two calls and two responses, got calls=%v resps=%v", callIDs, respIDs)
	}
	if callIDs[0] == callIDs[1] {
		t.Fatalf("two distinct calls must get distinct ids, both were %q", callIDs[0])
	}
	// FIFO pairing: response[i] matches call[i].
	if respIDs[0] != callIDs[0] || respIDs[1] != callIDs[1] {
		t.Fatalf("responses must pair with their calls in order; calls=%v resps=%v", callIDs, respIDs)
	}
}

// TestGeminiInterleavedCallsDoNotCollapse guards the specific regression: when a
// second call's name differs and responses interleave, no response collapses onto
// the positional last id. Each response resolves to the id of the call with its name.
func TestGeminiInterleavedCallsDoNotCollapse(t *testing.T) {
	req := `{
		"contents": [
			{"role": "model", "parts": [
				{"functionCall": {"name": "alpha", "args": {}}},
				{"functionCall": {"name": "beta", "args": {}}}
			]},
			{"role": "user", "parts": [
				{"functionResponse": {"name": "alpha", "response": {"content": "A"}}},
				{"functionResponse": {"name": "beta", "response": {"content": "B"}}}
			]}
		]
	}`

	out := ConvertGeminiRequestToOpenAI("m", []byte(req), false)

	callIDs, respIDs := collectToolCallIDs(t, out)
	if len(callIDs) != 2 || len(respIDs) != 2 {
		t.Fatalf("expected two calls and two responses, got calls=%v resps=%v", callIDs, respIDs)
	}
	// alpha -> call_alpha_0, beta -> call_beta_0; responses match by name (not last id).
	if callIDs[0] != "call_alpha_0" || callIDs[1] != "call_beta_0" {
		t.Fatalf("unexpected deterministic call ids: %v", callIDs)
	}
	if respIDs[0] != "call_alpha_0" {
		t.Fatalf("alpha response must resolve to call_alpha_0, got %q", respIDs[0])
	}
	if respIDs[1] != "call_beta_0" {
		t.Fatalf("beta response must NOT collapse to the positional last id; got %q", respIDs[1])
	}
}

// TestGeminiClientIDTakesPriorityOverMint verifies that even when ids COULD be
// minted, a client-provided id is used verbatim (priority 1 of the response branch).
func TestGeminiClientIDTakesPriorityOverMint(t *testing.T) {
	req := `{
		"contents": [
			{"role": "model", "parts": [
				{"functionCall": {"name": "do_thing", "args": {}}}
			]},
			{"role": "user", "parts": [
				{"functionResponse": {"id": "client_supplied", "name": "do_thing", "response": {"content": "ok"}}}
			]}
		]
	}`

	out := ConvertGeminiRequestToOpenAI("m", []byte(req), false)

	_, respIDs := collectToolCallIDs(t, out)
	if len(respIDs) != 1 || respIDs[0] != "client_supplied" {
		t.Fatalf("response id must take priority over a minted id, got %v", respIDs)
	}
}

// TestGeminiSanitizesNonAlnumNameInMintedID verifies the minted id is sanitized so a
// function name with separators (e.g. an MCP-namespaced name) yields a stable id.
func TestGeminiSanitizesNonAlnumNameInMintedID(t *testing.T) {
	req := `{
		"contents": [
			{"role": "model", "parts": [
				{"functionCall": {"name": "mcp.tool/run", "args": {}}}
			]},
			{"role": "user", "parts": [
				{"functionResponse": {"name": "mcp.tool/run", "response": {"content": "ok"}}}
			]}
		]
	}`

	out := ConvertGeminiRequestToOpenAI("m", []byte(req), false)

	callIDs, respIDs := collectToolCallIDs(t, out)
	if len(callIDs) != 1 || len(respIDs) != 1 {
		t.Fatalf("expected one call and one response, got calls=%v resps=%v", callIDs, respIDs)
	}
	if callIDs[0] != "call_mcp_tool_run_0" {
		t.Fatalf("expected sanitized deterministic id call_mcp_tool_run_0, got %q", callIDs[0])
	}
	if callIDs[0] != respIDs[0] {
		t.Fatalf("sanitized call/response ids must agree; call=%q resp=%q", callIDs[0], respIDs[0])
	}
}
