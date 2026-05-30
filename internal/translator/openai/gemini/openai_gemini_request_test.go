package gemini

import (
	"strings"
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

// lastUserContentParts returns the content array of the last role:"user" message in the
// converted OpenAI request. Used by the ADD-54 fileData tests.
func lastUserContentParts(t *testing.T, out []byte) gjson.Result {
	t.Helper()
	var found gjson.Result
	gjson.GetBytes(out, "messages").ForEach(func(_, msg gjson.Result) bool {
		if msg.Get("role").String() == "user" {
			found = msg.Get("content")
		}
		return true
	})
	return found
}

// TestGeminiFileDataImageBecomesImageURL (ADD-54) verifies a Gemini fileData image part
// (media referenced by URI, camelCase wire form) is translated to an OpenAI image_url
// content part carrying the URI verbatim — not silently dropped.
func TestGeminiFileDataImageBecomesImageURL(t *testing.T) {
	req := `{
		"contents": [
			{"role": "user", "parts": [
				{"text": "what is this?"},
				{"fileData": {"fileUri": "https://files.example/img.png", "mimeType": "image/png"}}
			]}
		]
	}`

	out := ConvertGeminiRequestToOpenAI("m", []byte(req), false)

	content := lastUserContentParts(t, out)
	if !content.IsArray() {
		t.Fatalf("expected structured content array (image present), got: %s", out)
	}
	var gotImageURL string
	content.ForEach(func(_, p gjson.Result) bool {
		if p.Get("type").String() == "image_url" {
			gotImageURL = p.Get("image_url.url").String()
		}
		return true
	})
	if gotImageURL != "https://files.example/img.png" {
		t.Fatalf("expected fileData image URI carried into image_url.url, got %q: %s", gotImageURL, out)
	}
}

// TestGeminiFileDataSnakeCaseImageBecomesImageURL (ADD-54) verifies the snake_case wire
// form some SDKs emit (file_data.file_uri / file_data.mime_type) is also translated.
func TestGeminiFileDataSnakeCaseImageBecomesImageURL(t *testing.T) {
	req := `{
		"contents": [
			{"role": "user", "parts": [
				{"file_data": {"file_uri": "gs://bucket/photo.jpg", "mime_type": "image/jpeg"}}
			]}
		]
	}`

	out := ConvertGeminiRequestToOpenAI("m", []byte(req), false)

	content := lastUserContentParts(t, out)
	if !content.IsArray() || content.Get("0.type").String() != "image_url" {
		t.Fatalf("expected snake_case file_data image -> image_url, got: %s", out)
	}
	if got := content.Get("0.image_url.url").String(); got != "gs://bucket/photo.jpg" {
		t.Fatalf("expected file_uri carried into image_url.url, got %q: %s", got, out)
	}
}

// TestGeminiFileDataNonImageNotSilentlyDropped (ADD-54) verifies that a non-image fileData
// part (e.g. a PDF by URI) is NOT silently dropped: it must surface as a visible text
// marker so the model can see an attachment was referenced. The dominant failure mode is
// false success — never silently omit media.
func TestGeminiFileDataNonImageNotSilentlyDropped(t *testing.T) {
	req := `{
		"contents": [
			{"role": "user", "parts": [
				{"text": "summarize"},
				{"fileData": {"fileUri": "https://files.example/doc.pdf", "mimeType": "application/pdf"}}
			]}
		]
	}`

	out := ConvertGeminiRequestToOpenAI("m", []byte(req), false)

	content := lastUserContentParts(t, out)
	if !content.IsArray() {
		t.Fatalf("expected structured content array, got: %s", out)
	}
	var markerSeen bool
	content.ForEach(func(_, p gjson.Result) bool {
		if p.Get("type").String() == "text" {
			txt := p.Get("text").String()
			if strings.Contains(txt, "doc.pdf") && strings.Contains(txt, "application/pdf") {
				markerSeen = true
			}
		}
		// The non-image attachment must NOT become a bogus image_url.
		if p.Get("type").String() == "image_url" {
			t.Fatalf("non-image fileData must not become image_url: %s", out)
		}
		return true
	})
	if !markerSeen {
		t.Fatalf("expected a visible unsupported-attachment marker naming the PDF, got: %s", out)
	}
}

// TestGeminiFileDataImageOnlyTurnNotEmptied (ADD-54) verifies an image-only turn whose
// ONLY part is a non-image fileData (no text part) does not collapse to an empty string
// content. Before the fix the part was dropped and the turn could become an empty user
// turn; the marker must be preserved via the structured content array.
func TestGeminiFileDataImageOnlyTurnNotEmptied(t *testing.T) {
	req := `{
		"contents": [
			{"role": "user", "parts": [
				{"fileData": {"fileUri": "https://files.example/data.bin", "mimeType": "application/octet-stream"}}
			]}
		]
	}`

	out := ConvertGeminiRequestToOpenAI("m", []byte(req), false)

	content := lastUserContentParts(t, out)
	// Must be a non-empty structured array carrying the marker, not "" and not absent.
	if !content.IsArray() || len(content.Array()) == 0 {
		t.Fatalf("expected non-empty structured content for a fileData-only turn, got: %s", out)
	}
	if txt := content.Get("0.text").String(); !strings.Contains(txt, "data.bin") {
		t.Fatalf("expected the fileData marker to survive for a fileData-only turn, got: %s", out)
	}
}

// TestGeminiFileDataMissingURIMarker (ADD-54) verifies a fileData part with no usable URI
// still surfaces a visible marker (never a silent drop, never an empty image_url).
func TestGeminiFileDataMissingURIMarker(t *testing.T) {
	req := `{
		"contents": [
			{"role": "user", "parts": [
				{"text": "look"},
				{"fileData": {"mimeType": "image/png"}}
			]}
		]
	}`

	out := ConvertGeminiRequestToOpenAI("m", []byte(req), false)

	content := lastUserContentParts(t, out)
	content.ForEach(func(_, p gjson.Result) bool {
		if p.Get("type").String() == "image_url" {
			if p.Get("image_url.url").String() == "" {
				t.Fatalf("missing-URI fileData must not produce an empty image_url: %s", out)
			}
		}
		return true
	})
	var markerSeen bool
	content.ForEach(func(_, p gjson.Result) bool {
		if p.Get("type").String() == "text" && strings.Contains(p.Get("text").String(), "missing file URI") {
			markerSeen = true
		}
		return true
	})
	if !markerSeen {
		t.Fatalf("expected a missing-URI marker, got: %s", out)
	}
}

// TestGeminiFileDataInSystemInstruction (ADD-54) verifies fileData parts in the
// systemInstruction are also handled (image -> image_url), not dropped.
func TestGeminiFileDataInSystemInstruction(t *testing.T) {
	req := `{
		"systemInstruction": {"parts": [
			{"text": "you are a vision model"},
			{"fileData": {"fileUri": "https://files.example/ref.webp", "mimeType": "image/webp"}}
		]},
		"contents": [{"role": "user", "parts": [{"text": "hi"}]}]
	}`

	out := ConvertGeminiRequestToOpenAI("m", []byte(req), false)

	sys := gjson.GetBytes(out, "messages.0")
	if sys.Get("role").String() != "system" {
		t.Fatalf("expected first message to be system, got: %s", out)
	}
	var gotURL string
	sys.Get("content").ForEach(func(_, p gjson.Result) bool {
		if p.Get("type").String() == "image_url" {
			gotURL = p.Get("image_url.url").String()
		}
		return true
	})
	if gotURL != "https://files.example/ref.webp" {
		t.Fatalf("expected systemInstruction fileData image carried to image_url, got %q: %s", gotURL, out)
	}
}

// TestGeminiSamplingFieldsStillMapped (ADD-72 dependency) guards the contract the composer
// executor relies on: this request translator must keep emitting temperature/top_p/n from
// generationConfig onto the OpenAI body. The ADD-72 fix (detecting present-but-unsupported
// sampling and recording unsupportedHardGuarantees) lives in the executor's
// composerConstraints and reads these fields; if this mapping were removed the executor
// could no longer detect them. This test ensures the mapping is not regressed here.
func TestGeminiSamplingFieldsStillMapped(t *testing.T) {
	req := `{
		"contents": [{"role": "user", "parts": [{"text": "go"}]}],
		"generationConfig": {"temperature": 0.3, "topP": 0.9, "candidateCount": 2}
	}`

	out := ConvertGeminiRequestToOpenAI("m", []byte(req), false)

	if got := gjson.GetBytes(out, "temperature").Float(); got != 0.3 {
		t.Fatalf("expected temperature=0.3 mapped, got %v: %s", got, out)
	}
	if got := gjson.GetBytes(out, "top_p").Float(); got != 0.9 {
		t.Fatalf("expected top_p=0.9 mapped, got %v: %s", got, out)
	}
	if got := gjson.GetBytes(out, "n").Int(); got != 2 {
		t.Fatalf("expected candidateCount->n=2 mapped, got %v: %s", got, out)
	}
}

// lastToolContent returns the raw `content` field (as a gjson.Result against the
// converted body) of the final role:"tool" message. Used by the ADD-85 tests so they
// can distinguish a string content ("hello") from an embedded JSON value.
func lastToolContent(t *testing.T, out []byte) gjson.Result {
	t.Helper()
	var found gjson.Result
	gjson.GetBytes(out, "messages").ForEach(func(_, msg gjson.Result) bool {
		if msg.Get("role").String() == "tool" {
			found = msg.Get("content")
		}
		return true
	})
	return found
}

// TestGeminiFunctionResponseContentTypes is the ADD-85 table test. A Gemini
// functionResponse.response.content can be a string, object, array, number, or bool. The
// OpenAI role:"tool" `content` field is always a STRING (that is the OpenAI shape). The
// fix's correctness property is about HOW that string is built:
//   - a STRING input must yield the bare unquoted text "hello" (NOT the double-encoded
//     JSON string "\"hello\"" the old code produced) — content.Raw == `"hello"`.
//   - object/array/number/bool inputs keep their raw JSON verbatim AS the content string
//     (the content string's value equals the raw JSON text, e.g. `{"k":"v"}`), never a
//     re-encoding of a quoted JSON-string-of-a-JSON-string.
func TestGeminiFunctionResponseContentTypes(t *testing.T) {
	cases := []struct {
		name        string
		contentJSON string
		// isString marks the string case (its content.Raw must be exactly `"hello"`).
		isString bool
		// wantContentValue is the decoded string value the content field must carry.
		wantContentValue string
	}{
		{name: "string", contentJSON: `"hello"`, isString: true, wantContentValue: "hello"},
		{name: "object", contentJSON: `{"k":"v"}`, wantContentValue: `{"k":"v"}`},
		{name: "array", contentJSON: `[1,2,3]`, wantContentValue: `[1,2,3]`},
		{name: "number", contentJSON: `42`, wantContentValue: `42`},
		{name: "bool", contentJSON: `true`, wantContentValue: `true`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := `{
				"contents": [
					{"role": "user", "parts": [
						{"functionResponse": {"id": "call_x", "name": "fn", "response": {"content": ` + tc.contentJSON + `}}}
					]}
				]
			}`
			out := ConvertGeminiRequestToOpenAI("m", []byte(req), false)
			content := lastToolContent(t, out)
			// The OpenAI tool content field is always a JSON string.
			if content.Type != gjson.String {
				t.Fatalf("content type = %v, want String (OpenAI tool content is a string): raw=%s: %s", content.Type, content.Raw, out)
			}
			if content.String() != tc.wantContentValue {
				t.Fatalf("content value = %q, want %q: %s", content.String(), tc.wantContentValue, out)
			}
			if tc.isString {
				// Guard the specific regression: a string input must NOT be double-encoded.
				// The serialized field is `"hello"`, never `"\"hello\""`.
				if content.Raw != `"hello"` {
					t.Fatalf("string content raw = %s, want \"hello\" (no double-encoding): %s", content.Raw, out)
				}
			}
		})
	}
}

// TestGeminiFunctionResponseStringResponseNoContentField (ADD-85, else branch) verifies
// that when response has no `content` field and the whole `response` is itself a STRING,
// the tool content is emitted unquoted too (not double-encoded). Note: Gemini's
// functionResponse.response is normally an object, but a defensive string is handled.
func TestGeminiFunctionResponseStringResponseNoContentField(t *testing.T) {
	req := `{
		"contents": [
			{"role": "user", "parts": [
				{"functionResponse": {"id": "call_y", "name": "fn", "response": "plain"}}
			]}
		]
	}`
	out := ConvertGeminiRequestToOpenAI("m", []byte(req), false)
	content := lastToolContent(t, out)
	if content.Type != gjson.String || content.String() != "plain" {
		t.Fatalf("expected unquoted string response content \"plain\", got type=%v value=%q: %s", content.Type, content.String(), out)
	}
}

// TestGeminiFunctionResponseObjectResponseNoContentField (ADD-85, else branch) verifies
// an object `response` with no `content` field keeps its raw JSON verbatim as the tool
// content string (the OpenAI content field is a string carrying the raw JSON text).
func TestGeminiFunctionResponseObjectResponseNoContentField(t *testing.T) {
	req := `{
		"contents": [
			{"role": "user", "parts": [
				{"functionResponse": {"id": "call_z", "name": "fn", "response": {"ok": true, "n": 5}}}
			]}
		]
	}`
	out := ConvertGeminiRequestToOpenAI("m", []byte(req), false)
	content := lastToolContent(t, out)
	if content.Type != gjson.String {
		t.Fatalf("expected tool content to be a string, got type=%v raw=%s: %s", content.Type, content.Raw, out)
	}
	// The string value is the raw JSON object; re-parse it and confirm the fields survive.
	inner := gjson.Parse(content.String())
	if inner.Get("ok").Bool() != true || inner.Get("n").Int() != 5 {
		t.Fatalf("expected object fields preserved in raw-JSON content string, got %q: %s", content.String(), out)
	}
}

// TestGeminiMultipleTextPartsJoinedWithNewline is the ADD-86 (gemini half) regression:
// two flattened text parts [{text:"a"},{text:"b"}] must become the string content "a\nb"
// — preserving the semantic boundary — not the delimiter-less "ab".
func TestGeminiMultipleTextPartsJoinedWithNewline(t *testing.T) {
	req := `{
		"contents": [
			{"role": "user", "parts": [{"text": "a"}, {"text": "b"}]}
		]
	}`
	out := ConvertGeminiRequestToOpenAI("m", []byte(req), false)
	last := lastMessage(t, out)
	if got := last.Get("content").String(); got != "a\nb" {
		t.Fatalf("expected flattened text content %q, got %q: %s", "a\nb", got, out)
	}
}

// TestGeminiSingleTextPartUnchanged (ADD-86 guard) verifies the delimiter is only
// inserted BETWEEN parts: a single text part must not gain a leading "\n".
func TestGeminiSingleTextPartUnchanged(t *testing.T) {
	req := `{"contents": [{"role": "user", "parts": [{"text": "solo"}]}]}`
	out := ConvertGeminiRequestToOpenAI("m", []byte(req), false)
	last := lastMessage(t, out)
	if got := last.Get("content").String(); got != "solo" {
		t.Fatalf("expected single text part %q unchanged, got %q: %s", "solo", got, out)
	}
}

// TestGeminiThinkingBudgetMapsToReasoningEffort is the ADD-87 (gemini half) test: a
// generationConfig.thinkingConfig.thinkingBudget must map to reasoning_effort (the
// executor's ADD-87 fix then carries reasoning_effort to the bridge). A budget of 8192
// falls in the "medium" band (1025..8192) per thinking.ConvertBudgetToLevel.
func TestGeminiThinkingBudgetMapsToReasoningEffort(t *testing.T) {
	req := `{
		"contents": [{"role": "user", "parts": [{"text": "go"}]}],
		"generationConfig": {"thinkingConfig": {"thinkingBudget": 8192}}
	}`
	out := ConvertGeminiRequestToOpenAI("m", []byte(req), false)
	if got := gjson.GetBytes(out, "reasoning_effort").String(); got != "medium" {
		t.Fatalf("expected thinkingBudget=8192 -> reasoning_effort=medium, got %q: %s", got, out)
	}
}

// TestGeminiThinkingBudgetSnakeCaseMapsToReasoningEffort (ADD-87 guard) verifies the
// snake_case thinking_budget alias (Google's official Python SDK shape) is also mapped.
func TestGeminiThinkingBudgetSnakeCaseMapsToReasoningEffort(t *testing.T) {
	req := `{
		"contents": [{"role": "user", "parts": [{"text": "go"}]}],
		"generationConfig": {"thinkingConfig": {"thinking_budget": 0}}
	}`
	out := ConvertGeminiRequestToOpenAI("m", []byte(req), false)
	// budget 0 -> "none" per ConvertBudgetToLevel.
	if got := gjson.GetBytes(out, "reasoning_effort").String(); got != "none" {
		t.Fatalf("expected thinking_budget=0 -> reasoning_effort=none, got %q: %s", got, out)
	}
}

// TestGeminiOmittedRoleDefaultsToUser is the ADD-91 [RBT-028] regression: a content
// with no role at all (a valid Gemini shape) must produce an OpenAI message with
// role:"user" carrying the real text, so Composer finds the user turn and does NOT send
// an empty prompt.
func TestGeminiOmittedRoleDefaultsToUser(t *testing.T) {
	req := `{"contents": [{"parts": [{"text": "hi"}]}]}`
	out := ConvertGeminiRequestToOpenAI("m", []byte(req), false)
	last := lastMessage(t, out)
	if last.Get("role").String() != "user" {
		t.Fatalf("expected omitted role to default to user, got %q: %s", last.Get("role").String(), out)
	}
	if got := last.Get("content").String(); got != "hi" {
		t.Fatalf("expected user content 'hi' (not empty), got %q: %s", got, out)
	}
}

// TestGeminiEmptyRoleStringDefaultsToUser (ADD-91 guard) verifies an explicit empty/
// whitespace role string is also defaulted to user.
func TestGeminiEmptyRoleStringDefaultsToUser(t *testing.T) {
	req := `{"contents": [{"role": "  ", "parts": [{"text": "hey"}]}]}`
	out := ConvertGeminiRequestToOpenAI("m", []byte(req), false)
	last := lastMessage(t, out)
	if last.Get("role").String() != "user" {
		t.Fatalf("expected whitespace role to default to user, got %q: %s", last.Get("role").String(), out)
	}
}

// TestGeminiOmittedRoleWithFunctionCallDefaultsToAssistant (ADD-91 edge) verifies a
// role-less content whose parts are a model-side functionCall is emitted as an assistant
// tool_calls message — OpenAI requires role:"assistant" to carry tool_calls, so the
// empty-role default must be "assistant" (not "user") in that case.
func TestGeminiOmittedRoleWithFunctionCallDefaultsToAssistant(t *testing.T) {
	req := `{
		"contents": [
			{"parts": [{"functionCall": {"id": "c1", "name": "search", "args": {"q": "x"}}}]},
			{"role": "user", "parts": [
				{"functionResponse": {"id": "c1", "name": "search", "response": {"content": "ok"}}}
			]}
		]
	}`
	out := ConvertGeminiRequestToOpenAI("m", []byte(req), false)
	// The assistant tool_call message must carry role:"assistant" and the tool_call.
	var assistantWithToolCall bool
	gjson.GetBytes(out, "messages").ForEach(func(_, msg gjson.Result) bool {
		if msg.Get("role").String() == "assistant" && msg.Get("tool_calls.0.id").String() == "c1" {
			assistantWithToolCall = true
		}
		return true
	})
	if !assistantWithToolCall {
		t.Fatalf("expected role-less functionCall content to become an assistant tool_call message: %s", out)
	}
	// And it must NOT have been emitted as a user message.
	gjson.GetBytes(out, "messages").ForEach(func(_, msg gjson.Result) bool {
		if msg.Get("role").String() == "user" && msg.Get("tool_calls").Exists() {
			t.Fatalf("functionCall content must not be a user tool_call message: %s", out)
		}
		return true
	})
}

// TestGeminiResponseMimeTypeOnlyMapsToJSONObject is the ADD-93 [Comment 3] test: a
// generationConfig.responseMimeType of application/json with NO schema maps to
// response_format {type:"json_object"} so the executor's composerConstraints sees it.
func TestGeminiResponseMimeTypeOnlyMapsToJSONObject(t *testing.T) {
	req := `{
		"contents": [{"role": "user", "parts": [{"text": "json please"}]}],
		"generationConfig": {"responseMimeType": "application/json"}
	}`
	out := ConvertGeminiRequestToOpenAI("m", []byte(req), false)
	rf := gjson.GetBytes(out, "response_format")
	if !rf.Exists() {
		t.Fatalf("expected response_format to be emitted: %s", out)
	}
	if rf.Get("type").String() != "json_object" {
		t.Fatalf("expected response_format.type=json_object, got %q: %s", rf.Get("type").String(), out)
	}
	if rf.Get("json_schema").Exists() {
		t.Fatalf("json_object must not carry a json_schema: %s", out)
	}
}

// TestGeminiResponseMimeTypeWithSchemaMapsToJSONSchema (ADD-93) verifies that a
// responseMimeType + responseSchema maps to response_format {type:"json_schema",
// json_schema:{schema:<schema>}} with the schema embedded verbatim.
func TestGeminiResponseMimeTypeWithSchemaMapsToJSONSchema(t *testing.T) {
	req := `{
		"contents": [{"role": "user", "parts": [{"text": "recipes"}]}],
		"generationConfig": {
			"responseMimeType": "application/json",
			"responseSchema": {"type": "array", "items": {"type": "object", "properties": {"recipeName": {"type": "string"}}, "required": ["recipeName"]}}
		}
	}`
	out := ConvertGeminiRequestToOpenAI("m", []byte(req), false)
	rf := gjson.GetBytes(out, "response_format")
	if rf.Get("type").String() != "json_schema" {
		t.Fatalf("expected response_format.type=json_schema, got %q: %s", rf.Get("type").String(), out)
	}
	schema := rf.Get("json_schema.schema")
	if !schema.Exists() {
		t.Fatalf("expected the schema embedded under json_schema.schema: %s", out)
	}
	if schema.Get("type").String() != "array" {
		t.Fatalf("expected embedded schema.type=array, got %q: %s", schema.Get("type").String(), out)
	}
	if schema.Get("items.properties.recipeName.type").String() != "string" {
		t.Fatalf("expected embedded schema preserved verbatim, got %s: %s", schema.Raw, out)
	}
}

// TestGeminiResponseMimeTypeSnakeCaseAliases (ADD-93) verifies the snake_case SDK
// aliases response_mime_type / response_schema are honored.
func TestGeminiResponseMimeTypeSnakeCaseAliases(t *testing.T) {
	req := `{
		"contents": [{"role": "user", "parts": [{"text": "json"}]}],
		"generationConfig": {
			"response_mime_type": "application/json",
			"response_schema": {"type": "object", "properties": {"ok": {"type": "boolean"}}}
		}
	}`
	out := ConvertGeminiRequestToOpenAI("m", []byte(req), false)
	rf := gjson.GetBytes(out, "response_format")
	if rf.Get("type").String() != "json_schema" {
		t.Fatalf("expected snake_case aliases -> json_schema, got %q: %s", rf.Get("type").String(), out)
	}
	if rf.Get("json_schema.schema.properties.ok.type").String() != "boolean" {
		t.Fatalf("expected snake_case response_schema embedded, got %s: %s", rf.Raw, out)
	}
}

// TestGeminiNonJSONResponseMimeTypeNoResponseFormat (ADD-93 guard) verifies a non-JSON
// responseMimeType (e.g. text/plain) does NOT emit a response_format.
func TestGeminiNonJSONResponseMimeTypeNoResponseFormat(t *testing.T) {
	req := `{
		"contents": [{"role": "user", "parts": [{"text": "go"}]}],
		"generationConfig": {"responseMimeType": "text/plain"}
	}`
	out := ConvertGeminiRequestToOpenAI("m", []byte(req), false)
	if gjson.GetBytes(out, "response_format").Exists() {
		t.Fatalf("text/plain responseMimeType must not emit response_format: %s", out)
	}
}
