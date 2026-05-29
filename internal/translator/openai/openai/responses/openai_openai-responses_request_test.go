package responses

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertOpenAIResponsesRequestToOpenAIChatCompletions_StringContent(t *testing.T) {
	payload := []byte(`{
		"model": "copilot-gpt-4.1",
		"input": [
			{"role":"system","content":"stay concise"},
			{"role":"user","content":"hello there"}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("copilot-gpt-4.1", payload, false)

	msgs := gjson.GetBytes(out, "messages")
	if !msgs.IsArray() {
		t.Fatalf("messages not array: %s", msgs.Raw)
	}

	system := msgs.Array()[0]
	if got := system.Get("role").String(); got != "system" {
		t.Fatalf("system role = %q, want system", got)
	}
	if got := system.Get("content").String(); got != "stay concise" {
		t.Fatalf("system content = %q, want stay concise", got)
	}

	user := msgs.Array()[1]
	if got := user.Get("role").String(); got != "user" {
		t.Fatalf("user role = %q, want user", got)
	}
	if got := user.Get("content").String(); got != "hello there" {
		t.Fatalf("user content = %q, want hello there", got)
	}
}

func prettyJSONForTest(raw []byte) string {
	if !gjson.ValidBytes(raw) {
		return string(raw)
	}
	var out bytes.Buffer
	if err := json.Indent(&out, raw, "", "  "); err != nil {
		return string(raw)
	}
	return out.String()
}

func TestConvertOpenAIResponsesRequestToOpenAIChatCompletions_MergeConsecutiveFunctionCalls(t *testing.T) {
	raw := []byte(`{
		"input": [
			{"type":"function_call","call_id":"exec_command:0","name":"exec_command","arguments":"{\"cmd\":\"ls\"}"},
			{"type":"function_call","call_id":"exec_command:1","name":"exec_command","arguments":"{\"cmd\":\"pwd\"}"},
			{"type":"function_call_output","call_id":"exec_command:0","output":"ok0"},
			{"type":"function_call_output","call_id":"exec_command:1","output":"ok1"}
		]
	}`)
	t.Logf("input json:\n%s", prettyJSONForTest(raw))

	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("kimi-k2.6", raw, true)
	t.Logf("output json:\n%s", prettyJSONForTest(out))

	msgs := gjson.GetBytes(out, "messages")
	if !msgs.Exists() || !msgs.IsArray() {
		t.Fatalf("messages should be an array")
	}
	if got := len(msgs.Array()); got != 3 {
		t.Fatalf("messages count = %d, want %d", got, 3)
	}

	if got := gjson.GetBytes(out, "messages.0.role").String(); got != "assistant" {
		t.Fatalf("messages.0.role = %q, want %q", got, "assistant")
	}
	if got := len(gjson.GetBytes(out, "messages.0.tool_calls").Array()); got != 2 {
		t.Fatalf("messages.0.tool_calls length = %d, want %d", got, 2)
	}
	if got := gjson.GetBytes(out, "messages.0.tool_calls.0.id").String(); got != "exec_command:0" {
		t.Fatalf("messages.0.tool_calls.0.id = %q, want %q", got, "exec_command:0")
	}
	if got := gjson.GetBytes(out, "messages.0.tool_calls.1.id").String(); got != "exec_command:1" {
		t.Fatalf("messages.0.tool_calls.1.id = %q, want %q", got, "exec_command:1")
	}

	if got := gjson.GetBytes(out, "messages.1.tool_call_id").String(); got != "exec_command:0" {
		t.Fatalf("messages.1.tool_call_id = %q, want %q", got, "exec_command:0")
	}
	if got := gjson.GetBytes(out, "messages.2.tool_call_id").String(); got != "exec_command:1" {
		t.Fatalf("messages.2.tool_call_id = %q, want %q", got, "exec_command:1")
	}
}

func TestConvertOpenAIResponsesRequestToOpenAIChatCompletions_SplitFunctionCallsWhenInterrupted(t *testing.T) {
	raw := []byte(`{
		"input": [
			{"type":"function_call","call_id":"call_a","name":"tool_a","arguments":"{}"},
			{"type":"message","role":"user","content":"next"},
			{"type":"function_call","call_id":"call_b","name":"tool_b","arguments":"{}"}
		]
	}`)
	t.Logf("input json:\n%s", prettyJSONForTest(raw))

	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("kimi-k2.6", raw, false)
	t.Logf("output json:\n%s", prettyJSONForTest(out))

	if got := len(gjson.GetBytes(out, "messages").Array()); got != 3 {
		t.Fatalf("messages count = %d, want %d", got, 3)
	}
	if got := gjson.GetBytes(out, "messages.0.tool_calls.0.id").String(); got != "call_a" {
		t.Fatalf("messages.0.tool_calls.0.id = %q, want %q", got, "call_a")
	}
	if got := gjson.GetBytes(out, "messages.2.tool_calls.0.id").String(); got != "call_b" {
		t.Fatalf("messages.2.tool_calls.0.id = %q, want %q", got, "call_b")
	}
}

func TestConvertOpenAIResponsesRequestToOpenAIChatCompletions_DefersMessageUntilToolOutput(t *testing.T) {
	raw := []byte(`{
		"input": [
			{"type":"function_call","call_id":"call_x","name":"exec_command","arguments":"{\"cmd\":\"echo hi\"}"},
			{"type":"message","role":"user","content":"Approved command prefix saved"},
			{"type":"function_call_output","call_id":"call_x","output":"ok"},
			{"type":"message","role":"user","content":"next"}
		]
	}`)
	t.Logf("input json:\n%s", prettyJSONForTest(raw))

	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("kimi-k2.6", raw, true)
	t.Logf("output json:\n%s", prettyJSONForTest(out))

	if got := len(gjson.GetBytes(out, "messages").Array()); got != 4 {
		t.Fatalf("messages count = %d, want %d", got, 4)
	}
	if got := gjson.GetBytes(out, "messages.0.role").String(); got != "assistant" {
		t.Fatalf("messages.0.role = %q, want %q", got, "assistant")
	}
	if got := gjson.GetBytes(out, "messages.1.role").String(); got != "tool" {
		t.Fatalf("messages.1.role = %q, want %q", got, "tool")
	}
	if got := gjson.GetBytes(out, "messages.1.tool_call_id").String(); got != "call_x" {
		t.Fatalf("messages.1.tool_call_id = %q, want %q", got, "call_x")
	}
	if got := gjson.GetBytes(out, "messages.2.role").String(); got != "user" {
		t.Fatalf("messages.2.role = %q, want %q", got, "user")
	}
	if got := gjson.GetBytes(out, "messages.2.content").String(); got != "Approved command prefix saved" {
		t.Fatalf("messages.2.content = %q, want %q", got, "Approved command prefix saved")
	}
	if got := gjson.GetBytes(out, "messages.3.content").String(); got != "next" {
		t.Fatalf("messages.3.content = %q, want %q", got, "next")
	}
}

// H07 / C-TOOLCHOICE: a STRING tool_choice ("auto"/"none"/"required") must pass through
// verbatim as a string so the executor's extractComposerToolChoice matches it directly.
func TestConvertOpenAIResponsesRequest_ToolChoiceString(t *testing.T) {
	for _, want := range []string{"auto", "none", "required"} {
		raw := []byte(`{"tool_choice":"` + want + `","input":[{"role":"user","content":"hi"}]}`)
		out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("m", raw, false)
		tc := gjson.GetBytes(out, "tool_choice")
		if tc.Type != gjson.String {
			t.Fatalf("tool_choice %q: type = %v, want string (out=%s)", want, tc.Type, prettyJSONForTest(out))
		}
		if got := tc.String(); got != want {
			t.Fatalf("tool_choice = %q, want %q", got, want)
		}
	}
}

// H07 / C-TOOLCHOICE: an OBJECT function tool_choice already in Chat Completions shape
// {"type":"function","function":{"name":"Bash"}} must be emitted as an OBJECT (not the old
// lossy .String()), so the executor reads tool_choice.function.name -> "specific:Bash".
func TestConvertOpenAIResponsesRequest_ToolChoiceFunctionObject(t *testing.T) {
	raw := []byte(`{"tool_choice":{"type":"function","function":{"name":"Bash"}},"input":[{"role":"user","content":"hi"}]}`)
	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("m", raw, false)
	tc := gjson.GetBytes(out, "tool_choice")
	if !tc.IsObject() {
		t.Fatalf("tool_choice not object: %s", prettyJSONForTest(out))
	}
	if got := tc.Get("type").String(); got != "function" {
		t.Fatalf("tool_choice.type = %q, want function", got)
	}
	if got := tc.Get("function.name").String(); got != "Bash" {
		t.Fatalf("tool_choice.function.name = %q, want Bash (out=%s)", got, prettyJSONForTest(out))
	}
}

// H07 / C-TOOLCHOICE: the Responses variant {"type":"function","name":"Bash"} (forced name
// at the top level) must be normalized so the name moves under function.name -> the executor
// resolveComposerToolChoice yields specific:Bash.
func TestConvertOpenAIResponsesRequest_ToolChoiceFunctionTopLevelName(t *testing.T) {
	raw := []byte(`{"tool_choice":{"type":"function","name":"Bash"},"input":[{"role":"user","content":"hi"}]}`)
	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("m", raw, false)
	tc := gjson.GetBytes(out, "tool_choice")
	if !tc.IsObject() {
		t.Fatalf("tool_choice not object: %s", prettyJSONForTest(out))
	}
	if got := tc.Get("function.name").String(); got != "Bash" {
		t.Fatalf("tool_choice.function.name = %q, want Bash (out=%s)", got, prettyJSONForTest(out))
	}
	// The forced name must NOT be lost as a stringified blob.
	if tc.Type == gjson.String {
		t.Fatalf("tool_choice was stringified, want object: %s", tc.Raw)
	}
}

// H07 / C-TOOLCHOICE: the allowed_tools form must be carried as a first-class `allowed_tools`
// raw object for the executor to intersect with advertised tools, and tool_choice itself must
// stay "auto" (allowed_tools is a restriction set, not a single forced tool). Never widened.
func TestConvertOpenAIResponsesRequest_ToolChoiceAllowedTools(t *testing.T) {
	raw := []byte(`{"tool_choice":{"type":"allowed_tools","mode":"auto","tools":[{"type":"function","name":"Read"},{"type":"function","name":"Write"}]},"input":[{"role":"user","content":"hi"}]}`)
	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("m", raw, false)

	if got := gjson.GetBytes(out, "tool_choice").String(); got != "auto" {
		t.Fatalf("tool_choice = %q, want auto (out=%s)", got, prettyJSONForTest(out))
	}
	at := gjson.GetBytes(out, "allowed_tools")
	if !at.IsObject() {
		t.Fatalf("allowed_tools not carried as object: %s", prettyJSONForTest(out))
	}
	if got := at.Get("type").String(); got != "allowed_tools" {
		t.Fatalf("allowed_tools.type = %q, want allowed_tools", got)
	}
	if got := len(at.Get("tools").Array()); got != 2 {
		t.Fatalf("allowed_tools.tools length = %d, want 2", got)
	}
	if got := at.Get("tools.0.name").String(); got != "Read" {
		t.Fatalf("allowed_tools.tools.0.name = %q, want Read", got)
	}
}

// H16 / C-RESPID: previous_response_id and conversation_id must be surfaced onto the translated
// body so they stay readable for the executor's response-id -> sessionID map. The recorded key
// uses the same id the client passes back, so the durable agent resumes instead of one-shotting.
func TestConvertOpenAIResponsesRequest_SurfacesRespAndConvIDs(t *testing.T) {
	raw := []byte(`{"previous_response_id":"resp_abc123","conversation_id":"conv_xyz","input":[{"role":"user","content":"continue"}]}`)
	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("m", raw, false)
	if got := gjson.GetBytes(out, "previous_response_id").String(); got != "resp_abc123" {
		t.Fatalf("previous_response_id = %q, want resp_abc123 (out=%s)", got, prettyJSONForTest(out))
	}
	if got := gjson.GetBytes(out, "conversation_id").String(); got != "conv_xyz" {
		t.Fatalf("conversation_id = %q, want conv_xyz", got)
	}
}

// H16 / C-RESPID: the conversation may arrive as the `conversation` object form
// {"conversation":{"id":"conv_obj"}} or as a bare string; both must surface as conversation_id.
func TestConvertOpenAIResponsesRequest_ConversationObjectAndStringForms(t *testing.T) {
	objRaw := []byte(`{"conversation":{"id":"conv_obj"},"input":[{"role":"user","content":"hi"}]}`)
	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("m", objRaw, false)
	if got := gjson.GetBytes(out, "conversation_id").String(); got != "conv_obj" {
		t.Fatalf("conversation object form: conversation_id = %q, want conv_obj (out=%s)", got, prettyJSONForTest(out))
	}

	strRaw := []byte(`{"conversation":"conv_str","input":[{"role":"user","content":"hi"}]}`)
	out = ConvertOpenAIResponsesRequestToOpenAIChatCompletions("m", strRaw, false)
	if got := gjson.GetBytes(out, "conversation_id").String(); got != "conv_str" {
		t.Fatalf("conversation string form: conversation_id = %q, want conv_str", got)
	}
}

// H19: a Responses `developer` message must NOT be downgraded to `user`. Preserving the role
// keeps its elevated priority (downstream composer folds `developer` into the system prompt).
func TestConvertOpenAIResponsesRequest_PreservesDeveloperRole(t *testing.T) {
	raw := []byte(`{"input":[{"type":"message","role":"developer","content":[{"type":"input_text","text":"Follow the safety policy."}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("m", raw, false)
	if got := gjson.GetBytes(out, "messages.0.role").String(); got != "developer" {
		t.Fatalf("messages.0.role = %q, want developer (out=%s)", got, prettyJSONForTest(out))
	}
	if got := gjson.GetBytes(out, "messages.0.content.0.text").String(); got != "Follow the safety policy." {
		t.Fatalf("developer content lost: %s", prettyJSONForTest(out))
	}
}

// H20: parallel_tool_calls must be carried through verbatim (it is a best-effort / explicit-
// unsupported signal the executor consumes; the composer path cannot hard-cap Cursor's emission,
// but silently dropping the client's intent is wrong).
func TestConvertOpenAIResponsesRequest_CarriesParallelToolCalls(t *testing.T) {
	raw := []byte(`{"parallel_tool_calls":false,"input":[{"role":"user","content":"hi"}]}`)
	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("m", raw, false)
	v := gjson.GetBytes(out, "parallel_tool_calls")
	if !v.Exists() {
		t.Fatalf("parallel_tool_calls dropped: %s", prettyJSONForTest(out))
	}
	if v.Bool() != false {
		t.Fatalf("parallel_tool_calls = %v, want false", v.Bool())
	}
}

// H22: a Responses built-in tool (e.g. web_search) must NOT be silently dropped — it must remain
// in the tools array so a forced/allowed tool_choice stays consistent and the executor can surface
// an explicit unsupported signal rather than pretending the declared tool never existed.
func TestConvertOpenAIResponsesRequest_BuiltinToolsNotDropped(t *testing.T) {
	raw := []byte(`{"tools":[{"type":"web_search"},{"type":"function","name":"Bash","description":"run","parameters":{"type":"object"}}],"input":[{"role":"user","content":"search"}]}`)
	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("m", raw, false)
	tools := gjson.GetBytes(out, "tools")
	if !tools.IsArray() {
		t.Fatalf("tools not array: %s", prettyJSONForTest(out))
	}
	if got := len(tools.Array()); got != 2 {
		t.Fatalf("tools length = %d, want 2 (built-in must not be dropped): %s", got, prettyJSONForTest(out))
	}
	// The built-in tool must still be present (carried verbatim).
	var foundBuiltin bool
	for _, tool := range tools.Array() {
		if tool.Get("type").String() == "web_search" {
			foundBuiltin = true
		}
	}
	if !foundBuiltin {
		t.Fatalf("web_search built-in tool was dropped: %s", prettyJSONForTest(out))
	}
}

// L34: a structured (array) function_call_output.output must NOT be flattened lossily into a
// string. It is preserved as raw array content; the composer text extractor still reads `text`
// parts, and downstream renderers can pick up the structured parts.
func TestConvertOpenAIResponsesRequest_StructuredFunctionOutputPreserved(t *testing.T) {
	raw := []byte(`{"input":[
		{"type":"function_call","call_id":"c1","name":"Read","arguments":"{}"},
		{"type":"function_call_output","call_id":"c1","output":[{"type":"output_text","text":"hello"},{"type":"output_image","image_url":"data:img"}]}
	]}`)
	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("m", raw, false)

	// messages: [assistant(tool_calls), tool(structured content)]
	toolMsg := gjson.GetBytes(out, "messages.1")
	if got := toolMsg.Get("role").String(); got != "tool" {
		t.Fatalf("messages.1.role = %q, want tool (out=%s)", got, prettyJSONForTest(out))
	}
	content := toolMsg.Get("content")
	if !content.IsArray() {
		t.Fatalf("structured output flattened, content not array: %s", prettyJSONForTest(out))
	}
	if got := content.Get("0.text").String(); got != "hello" {
		t.Fatalf("structured text part lost: %s", prettyJSONForTest(out))
	}
	if got := content.Get("1.type").String(); got != "output_image" {
		t.Fatalf("structured image part lost: %s", prettyJSONForTest(out))
	}
}

// L34 (lossless string path): a plain-string function_call_output.output must stay a string
// (no behavior change for the common case).
func TestConvertOpenAIResponsesRequest_StringFunctionOutputStaysString(t *testing.T) {
	raw := []byte(`{"input":[
		{"type":"function_call","call_id":"c1","name":"Read","arguments":"{}"},
		{"type":"function_call_output","call_id":"c1","output":"plain result"}
	]}`)
	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("m", raw, false)
	content := gjson.GetBytes(out, "messages.1.content")
	if content.Type != gjson.String {
		t.Fatalf("string output should stay string, got type %v: %s", content.Type, prettyJSONForTest(out))
	}
	if got := content.String(); got != "plain result" {
		t.Fatalf("string output content = %q, want %q", got, "plain result")
	}
}
