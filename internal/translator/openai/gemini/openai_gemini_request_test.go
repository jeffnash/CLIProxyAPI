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

// lastMessage returns the final entry of the converted OpenAI `messages` array.
func lastMessage(t *testing.T, out []byte) gjson.Result {
	t.Helper()
	msgs := gjson.GetBytes(out, "messages").Array()
	if len(msgs) == 0 {
		t.Fatalf("expected at least one message, got none: %s", out)
	}
	return msgs[len(msgs)-1]
}

// countRole returns how many messages in the converted OpenAI body carry the role.
func countRole(out []byte, role string) int {
	n := 0
	gjson.GetBytes(out, "messages").ForEach(func(_, msg gjson.Result) bool {
		if msg.Get("role").String() == role {
			n++
		}
		return true
	})
	return n
}

// TestGeminiFunctionResponseOnlyContinuationDoesNotAppendEmptyUser is the H14
// regression: a continuation turn that contains ONLY functionResponse parts (no
// user text, no image, no functionCall) must NOT produce a trailing empty
// role:"user" message. The executor classifies a continuation by the LAST message
// being role:"tool"; an empty trailing user turn would instead look like a fresh
// (empty) user turn and the paused run would never receive the tool output.
func TestGeminiFunctionResponseOnlyContinuationDoesNotAppendEmptyUser(t *testing.T) {
	req := `{
		"contents": [
			{"role": "user", "parts": [{"text": "read both files"}]},
			{"role": "model", "parts": [
				{"functionCall": {"id": "call_a", "name": "read_file", "args": {"path": "a"}}},
				{"functionCall": {"id": "call_b", "name": "read_file", "args": {"path": "b"}}}
			]},
			{"role": "user", "parts": [
				{"functionResponse": {"id": "call_a", "name": "read_file", "response": {"content": "A"}}},
				{"functionResponse": {"id": "call_b", "name": "read_file", "response": {"content": "B"}}}
			]}
		]
	}`

	out := ConvertGeminiRequestToOpenAI("m", []byte(req), false)

	// The translated history must END on a role:"tool" message so the executor's
	// continuation detector classifies it as a tool_results continuation.
	if last := lastMessage(t, out); last.Get("role").String() != "tool" {
		t.Fatalf("expected last message role=tool (tool_results continuation), got %q: %s", last.Get("role").String(), out)
	}

	// No empty trailing user turn may exist: the only user message is the first one
	// (which has real text). There must be exactly one user message here.
	if got := countRole(out, "user"); got != 1 {
		t.Fatalf("expected exactly one (non-empty) user message, got %d: %s", got, out)
	}

	// Both tool responses must be present and carry their round-tripped ids.
	_, respIDs := collectToolCallIDs(t, out)
	if len(respIDs) != 2 || respIDs[0] != "call_a" || respIDs[1] != "call_b" {
		t.Fatalf("expected two tool responses [call_a call_b], got %v: %s", respIDs, out)
	}
}

// TestGeminiMixedToolResultPlusTextStillEmitsUserMessage guards the opposite of
// H14: a trailing turn that carries functionResponse parts AND user text is a mixed
// turn, not function-response-only. The user text must still be emitted (as a
// role:"user" message), so the H14 suppression does NOT swallow real trailing text.
func TestGeminiMixedToolResultPlusTextStillEmitsUserMessage(t *testing.T) {
	req := `{
		"contents": [
			{"role": "model", "parts": [
				{"functionCall": {"id": "toolu_x", "name": "get_weather", "args": {}}}
			]},
			{"role": "user", "parts": [
				{"functionResponse": {"id": "toolu_x", "name": "get_weather", "response": {"content": "rainy"}}},
				{"text": "thanks, now compare with yesterday"}
			]}
		]
	}`

	out := ConvertGeminiRequestToOpenAI("m", []byte(req), false)

	// The trailing user text must survive as a user message.
	if got := countRole(out, "user"); got != 1 {
		t.Fatalf("expected the trailing user text to be emitted as one user message, got %d: %s", got, out)
	}
	var userText string
	gjson.GetBytes(out, "messages").ForEach(func(_, msg gjson.Result) bool {
		if msg.Get("role").String() == "user" {
			userText = msg.Get("content").String()
		}
		return true
	})
	if userText != "thanks, now compare with yesterday" {
		t.Fatalf("expected trailing user text preserved, got %q: %s", userText, out)
	}
	// And the id still round-trips.
	_, respIDs := collectToolCallIDs(t, out)
	if len(respIDs) != 1 || respIDs[0] != "toolu_x" {
		t.Fatalf("expected tool response id toolu_x, got %v", respIDs)
	}
}

// TestGeminiEmptyUserTurnIsDropped verifies a content entry with no parts at all
// (a bare empty user turn) does not produce a phantom empty user message — same H14
// suppression path, generalized.
func TestGeminiEmptyUserTurnIsDropped(t *testing.T) {
	req := `{
		"contents": [
			{"role": "user", "parts": [{"text": "hello"}]},
			{"role": "user", "parts": []}
		]
	}`

	out := ConvertGeminiRequestToOpenAI("m", []byte(req), false)

	if got := countRole(out, "user"); got != 1 {
		t.Fatalf("expected exactly one non-empty user message (empty turn dropped), got %d: %s", got, out)
	}
	if last := lastMessage(t, out); last.Get("content").String() != "hello" {
		t.Fatalf("expected last user message to be the non-empty 'hello' turn, got %q", last.Get("content").String())
	}
}

// TestGeminiTwoParallelFunctionResponsesRoundTripExactIDs is the C02 read-back
// contract (C-GEMID, reader side): two parallel functionResponse parts carrying the
// exact ids the model emitted must map to two role:"tool" messages with those exact
// ids — no minting, no 1-pending collapse. This is the half this file owns; the
// response translator owns emitting functionCall.id on the wire.
func TestGeminiTwoParallelFunctionResponsesRoundTripExactIDs(t *testing.T) {
	req := `{
		"contents": [
			{"role": "model", "parts": [
				{"functionCall": {"id": "fc_1", "name": "search", "args": {"q": "a"}}},
				{"functionCall": {"id": "fc_2", "name": "search", "args": {"q": "b"}}}
			]},
			{"role": "user", "parts": [
				{"functionResponse": {"id": "fc_1", "name": "search", "response": {"content": "A"}}},
				{"functionResponse": {"id": "fc_2", "name": "search", "response": {"content": "B"}}}
			]}
		]
	}`

	out := ConvertGeminiRequestToOpenAI("m", []byte(req), false)

	callIDs, respIDs := collectToolCallIDs(t, out)
	if len(callIDs) != 2 || callIDs[0] != "fc_1" || callIDs[1] != "fc_2" {
		t.Fatalf("expected call ids [fc_1 fc_2] verbatim, got %v", callIDs)
	}
	if len(respIDs) != 2 || respIDs[0] != "fc_1" || respIDs[1] != "fc_2" {
		t.Fatalf("expected parallel response ids [fc_1 fc_2] echoed verbatim (no mint/collapse), got %v", respIDs)
	}
}

// TestGeminiAllowedFunctionNamesAnySingleMapsToSpecific is the H15 regression: mode
// ANY with exactly one allowedFunctionNames entry is the strongest constraint — force
// THAT tool. The executor consumes the `specific:<name>` token.
func TestGeminiAllowedFunctionNamesAnySingleMapsToSpecific(t *testing.T) {
	req := `{
		"contents": [{"role": "user", "parts": [{"text": "temp?"}]}],
		"toolConfig": {"functionCallingConfig": {"mode": "ANY", "allowedFunctionNames": ["get_current_temperature"]}}
	}`

	out := ConvertGeminiRequestToOpenAI("m", []byte(req), false)

	if tc := gjson.GetBytes(out, "tool_choice").String(); tc != "specific:get_current_temperature" {
		t.Fatalf("expected tool_choice=specific:get_current_temperature, got %q: %s", tc, out)
	}
	// A single forced tool does not also need an allowed_tools list.
	if gjson.GetBytes(out, "allowed_tools").Exists() {
		t.Fatalf("did not expect allowed_tools for a single forced tool: %s", out)
	}
}

// TestGeminiAllowedFunctionNamesAnyMultipleMapsToRequiredPlusAllowed verifies mode ANY
// with multiple allowed names keeps "required" (must call SOME tool) and carries the
// restricted set as allowed_tools (the C-TOOLCHOICE field the executor intersects with
// advertised tools). The allowed_tools shape mirrors the Responses allowed_tools form so
// the single shared executor consumer matches.
func TestGeminiAllowedFunctionNamesAnyMultipleMapsToRequiredPlusAllowed(t *testing.T) {
	req := `{
		"contents": [{"role": "user", "parts": [{"text": "go"}]}],
		"toolConfig": {"functionCallingConfig": {"mode": "ANY", "allowedFunctionNames": ["Read", "Grep"]}}
	}`

	out := ConvertGeminiRequestToOpenAI("m", []byte(req), false)

	if tc := gjson.GetBytes(out, "tool_choice").String(); tc != "required" {
		t.Fatalf("expected tool_choice=required for ANY with multiple allowed names, got %q: %s", tc, out)
	}
	at := gjson.GetBytes(out, "allowed_tools")
	if !at.Exists() {
		t.Fatalf("expected allowed_tools object for multiple allowed names: %s", out)
	}
	if at.Get("type").String() != "allowed_tools" {
		t.Fatalf("allowed_tools.type = %q, want allowed_tools: %s", at.Get("type").String(), out)
	}
	names := []string{}
	at.Get("tools").ForEach(func(_, tr gjson.Result) bool {
		if tr.Get("type").String() != "function" {
			t.Fatalf("allowed_tools tool ref must be type=function, got %q", tr.Get("type").String())
		}
		names = append(names, tr.Get("name").String())
		return true
	})
	if len(names) != 2 || names[0] != "Read" || names[1] != "Grep" {
		t.Fatalf("expected allowed_tools tools [Read Grep], got %v: %s", names, out)
	}
}

// TestGeminiAllowedFunctionNamesAutoSingleIsAllowNotForce verifies that a single allowed
// name under AUTO is an allow-restriction (the model may still answer in text), NOT a
// forced specific:<name>. So tool_choice stays "auto" and the single name is carried via
// allowed_tools.
func TestGeminiAllowedFunctionNamesAutoSingleIsAllowNotForce(t *testing.T) {
	req := `{
		"contents": [{"role": "user", "parts": [{"text": "go"}]}],
		"toolConfig": {"functionCallingConfig": {"mode": "AUTO", "allowedFunctionNames": ["Read"]}}
	}`

	out := ConvertGeminiRequestToOpenAI("m", []byte(req), false)

	if tc := gjson.GetBytes(out, "tool_choice").String(); tc != "auto" {
		t.Fatalf("expected tool_choice=auto (AUTO does not force), got %q: %s", tc, out)
	}
	at := gjson.GetBytes(out, "allowed_tools")
	if !at.Exists() || at.Get("tools.0.name").String() != "Read" {
		t.Fatalf("expected allowed_tools restricting to [Read] under AUTO, got: %s", out)
	}
}

// TestGeminiNoneModeIgnoresAllowedNames verifies mode NONE maps to tool_choice=none and
// never carries allowed_tools (the model cannot call any tool regardless of the list).
func TestGeminiNoneModeIgnoresAllowedNames(t *testing.T) {
	req := `{
		"contents": [{"role": "user", "parts": [{"text": "go"}]}],
		"toolConfig": {"functionCallingConfig": {"mode": "NONE", "allowedFunctionNames": ["Read"]}}
	}`

	out := ConvertGeminiRequestToOpenAI("m", []byte(req), false)

	if tc := gjson.GetBytes(out, "tool_choice").String(); tc != "none" {
		t.Fatalf("expected tool_choice=none, got %q: %s", tc, out)
	}
	if gjson.GetBytes(out, "allowed_tools").Exists() {
		t.Fatalf("NONE must not carry allowed_tools: %s", out)
	}
}

// TestGeminiModeOnlyNoAllowedNamesUnchanged verifies the pre-H15 behavior is preserved
// when no allowedFunctionNames is present: AUTO->auto, ANY->required, NONE->none, and no
// allowed_tools is added.
func TestGeminiModeOnlyNoAllowedNamesUnchanged(t *testing.T) {
	cases := map[string]string{"AUTO": "auto", "ANY": "required", "NONE": "none"}
	for mode, want := range cases {
		req := `{
			"contents": [{"role": "user", "parts": [{"text": "go"}]}],
			"toolConfig": {"functionCallingConfig": {"mode": "` + mode + `"}}
		}`
		out := ConvertGeminiRequestToOpenAI("m", []byte(req), false)
		if tc := gjson.GetBytes(out, "tool_choice").String(); tc != want {
			t.Fatalf("mode %s: expected tool_choice=%q, got %q", mode, want, tc)
		}
		if gjson.GetBytes(out, "allowed_tools").Exists() {
			t.Fatalf("mode %s: did not expect allowed_tools without allowedFunctionNames: %s", mode, out)
		}
	}
}
