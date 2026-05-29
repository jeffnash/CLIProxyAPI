package executor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func authWith(id, apiKey string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{ID: id, Attributes: map[string]string{"api_key": apiKey}}
}
func optsWithHeaders(h map[string]string) cliproxyexecutor.Options {
	hdr := http.Header{}
	for k, v := range h {
		hdr.Set(k, v)
	}
	return cliproxyexecutor.Options{Headers: hdr}
}

// toolTurn builds an OpenAI-shape payload for a real agentic turn (it advertises a tool), so it is NOT
// treated as a non-agentic utility one-shot — the stable-session contract applies to it. Use this where a
// test exercises stable/continuation routing rather than the utility-one-shot isolation path.
func toolTurn(text string) []byte {
	b, _ := json.Marshal(map[string]any{
		"tools":    []map[string]any{{"type": "function", "function": map[string]any{"name": "Read"}}},
		"messages": []map[string]any{{"role": "user", "content": text}},
	})
	return b
}

func TestDeriveComposerSessionID(t *testing.T) {
	convoOpts := func(conv string) cliproxyexecutor.Options {
		return optsWithHeaders(map[string]string{"X-Conversation-Id": conv})
	}
	// A stable conversation id routes an agentic turn deterministically and is independent of message text.
	a, err := deriveComposerSessionID(authWith("authA", "keyA"), toolTurn("hello"), convoOpts("conv-1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(a, "sess_") {
		t.Fatalf("session id should be prefixed sess_, got %s", a)
	}
	a2, _ := deriveComposerSessionID(authWith("authA", "keyA"), toolTurn("different text"), convoOpts("conv-1"))
	if a != a2 {
		t.Fatalf("same conversation id must be stable regardless of content: %s vs %s", a, a2)
	}
	// Different tenant, same conversation id => different id (no cross-tenant collision).
	b, _ := deriveComposerSessionID(authWith("authB", "keyB"), toolTurn("hello"), convoOpts("conv-1"))
	if a == b {
		t.Fatalf("different tenants must not share a session id")
	}
	// Different conversation id => different id.
	c, _ := deriveComposerSessionID(authWith("authA", "keyA"), toolTurn("hello"), convoOpts("conv-2"))
	if a == c {
		t.Fatalf("different conversation ids must differ")
	}
}

// A non-agentic utility one-shot (no tools advertised, not a tool_results turn) — e.g. a client's
// title-generation, topic-detection, quota, or conversation-summary call, which some clients fire
// CONCURRENTLY with the real turn under the SAME conversation id — must be isolated to a fresh ephemeral
// session. Otherwise it collides with the in-flight real turn on the conversation's stable session and the
// bridge rejects the loser with a 409 that surfaces to the client as a 500. The rule is purely structural
// (no tools) and client-agnostic; it deliberately also covers tool-less calls that DO carry history.
func TestDeriveComposerSessionID_UtilityOneShotIsolated(t *testing.T) {
	auth := authWith("authA", "keyA")
	conv := optsWithHeaders(map[string]string{"X-Conversation-Id": "shared-conv"})
	// Shape of a title request: system instruction + a single user message, zero tools.
	title := []byte(`{"messages":[{"role":"system","content":"Generate a concise, sentence-case title (3-7 words)"},{"role":"user","content":"the conversation so far"}]}`)
	// Shape of a summary/topic auxiliary call that carries history but advertises no tools — the old
	// "no tools AND no history" heuristic wrongly MISSED this; the structural "no tools" rule isolates it.
	summary := []byte(`{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"hello"},{"role":"user","content":"summarize the conversation above in one line"}]}`)

	// Utility one-shots on the SAME conversation id must NOT share a session (no per-session 409 race),
	// whether or not they carry history.
	o1, _ := deriveComposerSessionID(auth, title, conv)
	o2, _ := deriveComposerSessionID(auth, title, conv)
	o3, _ := deriveComposerSessionID(auth, summary, conv)
	if o1 == "" || o2 == "" || o3 == "" {
		t.Fatalf("utility one-shot must still route to a session (o1=%q o2=%q o3=%q)", o1, o2, o3)
	}
	if o1 == o2 || o1 == o3 || o2 == o3 {
		t.Fatalf("utility one-shots on the same conv must each get a distinct ephemeral session: %s %s %s", o1, o2, o3)
	}

	// The real agentic turn (advertises tools) on that SAME conversation id keeps the stable session, and
	// it must not collide with the isolated one-shots.
	r1, _ := deriveComposerSessionID(auth, toolTurn("describe this repo"), conv)
	r2, _ := deriveComposerSessionID(auth, toolTurn("describe this repo"), conv)
	if r1 != r2 {
		t.Fatalf("agentic turns on the same conv must share the stable session: %s vs %s", r1, r2)
	}
	if r1 == o1 || r1 == o2 || r1 == o3 {
		t.Fatalf("utility one-shot must not collide with the conversation's stable session")
	}

	// A continuation (last message is a tool result) is NOT a one-shot even with no tools advertised:
	// it must still route to the conversation's stable session (here via the stable conversation id).
	cont := []byte(`{"messages":[{"role":"assistant","tool_calls":[{"id":"tc_1"}]},{"role":"tool","tool_call_id":"tc_1","content":"R"}]}`)
	c1, err := deriveComposerSessionID(auth, cont, conv)
	if err != nil {
		t.Fatalf("continuation must route, not error: %v", err)
	}
	if c1 != r1 {
		t.Fatalf("continuation must route to the stable session (got %s want %s)", c1, r1)
	}
}

// A stateless new-user turn (no conversation id, no metadata.user_id — curl/SDK/simple UI) must NOT be
// rejected: it gets a fresh RANDOM session id (collision-free, unlike a content hash). Only an unroutable
// continuation errors. A stable metadata.user_id still routes deterministically and is content-independent.
func TestDeriveComposerSessionID_StatelessMint(t *testing.T) {
	auth := authWith("authA", "keyA")
	a, err := deriveComposerSessionID(auth, []byte(`{"messages":[{"role":"user","content":"hello"}]}`), cliproxyexecutor.Options{})
	if err != nil || !strings.HasPrefix(a, "sess_") {
		t.Fatalf("stateless new turn must mint a session, not error (id=%q err=%v)", a, err)
	}
	// Two distinct stateless conversations must NOT collide (fresh random per new turn).
	b, _ := deriveComposerSessionID(auth, []byte(`{"messages":[{"role":"user","content":"hello"}]}`), cliproxyexecutor.Options{})
	if a == b {
		t.Fatalf("distinct stateless new turns must get distinct minted ids")
	}
	// metadata.user_id (Claude session) routes an agentic turn deterministically and is content-independent.
	o := cliproxyexecutor.Options{OriginalRequest: []byte(`{"metadata":{"user_id":"user_x_account__session_uuid-1"}}`)}
	s1, err := deriveComposerSessionID(auth, toolTurn("hi"), o)
	if err != nil || s1 == "" {
		t.Fatalf("metadata.user_id should route: %v", err)
	}
	s1b, _ := deriveComposerSessionID(auth, toolTurn("different"), o)
	if s1 != s1b {
		t.Fatalf("same Claude session uuid must be stable regardless of content")
	}
	s2, _ := deriveComposerSessionID(auth, toolTurn("hi"), cliproxyexecutor.Options{OriginalRequest: []byte(`{"metadata":{"user_id":"user_x_account__session_uuid-2"}}`)})
	if s1 == s2 {
		t.Fatalf("different Claude session uuids must differ")
	}
}

// Comment 1: a continuation (tool_results) turn with no stable id routes by a previously emitted
// tool_call_id; an unknown tool_call_id with no stable id errors.
func TestDeriveComposerSessionID_ToolCallContinuation(t *testing.T) {
	auth := authWith("authA", "keyA")
	sid, err := deriveComposerSessionID(auth, toolTurn("start"), optsWithHeaders(map[string]string{"X-Conversation-Id": "conv-x"}))
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	recordComposerToolCall(composerTenant(auth, cliproxyexecutor.Options{}), "tc_42", sid)
	cont := []byte(`{"messages":[{"role":"assistant","tool_calls":[{"id":"tc_42"}]},{"role":"tool","tool_call_id":"tc_42","content":"R"}]}`)
	got, err := deriveComposerSessionID(auth, cont, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("continuation should route by tool_call_id: %v", err)
	}
	if got != sid {
		t.Fatalf("continuation routed to wrong session: %s vs %s", got, sid)
	}
	unknown := []byte(`{"messages":[{"role":"tool","tool_call_id":"tc_unknown","content":"R"}]}`)
	if _, err := deriveComposerSessionID(auth, unknown, cliproxyexecutor.Options{}); err == nil {
		t.Fatalf("unknown tool_call_id with no stable id must error")
	}
	// L1: the SAME tool_call_id under a DIFFERENT tenant must NOT resolve to the first tenant's session.
	if _, err := deriveComposerSessionID(authWith("authB", "keyB"), cont, cliproxyexecutor.Options{}); err == nil {
		t.Fatalf("cross-tenant tool_call_id reuse must not resolve (tenant-scoped map)")
	}
}

// Same conversation id but different inbound caller credential => different sessions (separates users
// who share one upstream Cursor key).
func TestDeriveComposerSessionID_CallerIsolation(t *testing.T) {
	auth := authWith("authA", "keyA")
	u1, _ := deriveComposerSessionID(auth, toolTurn("x"), optsWithHeaders(map[string]string{"X-Conversation-Id": "shared", "Authorization": "Bearer userone"}))
	u2, _ := deriveComposerSessionID(auth, toolTurn("x"), optsWithHeaders(map[string]string{"X-Conversation-Id": "shared", "Authorization": "Bearer usertwo"}))
	if u1 == u2 {
		t.Fatalf("distinct callers must get distinct sessions: %s == %s", u1, u2)
	}
}

func TestClaudeSessionID(t *testing.T) {
	// Old format: user_<acct>_account__session_<uuid>.
	if got := claudeSessionID([]byte(`{"metadata":{"user_id":"user_x_account__session_ac980658-1234"}}`)); got != "claude:ac980658-1234" {
		t.Fatalf("old-format session id: %q", got)
	}
	// New format: JSON object with session_id.
	if got := claudeSessionID([]byte(`{"metadata":{"user_id":"{\"device_id\":\"d\",\"session_id\":\"sid-9\"}"}}`)); got != "claude:sid-9" {
		t.Fatalf("new-format session id: %q", got)
	}
	// A bare user_id (no session) is per-account, not per-conversation => ignored here.
	if got := claudeSessionID([]byte(`{"metadata":{"user_id":"just-an-account"}}`)); got != "" {
		t.Fatalf("bare user_id must be ignored, got %q", got)
	}
	if got := claudeSessionID(nil); got != "" {
		t.Fatalf("empty payload => empty, got %q", got)
	}
}

func TestComposerInputClassification(t *testing.T) {
	// New user turn.
	user := composerInput([]byte(`{"messages":[{"role":"system","content":"be terse"},{"role":"user","content":"hi there"}]}`))
	if user["type"] != "user" {
		t.Fatalf("expected user turn, got %v", user["type"])
	}
	if user["text"] != "hi there" {
		t.Fatalf("expected user text, got %v", user["text"])
	}
	if user["system"] != "be terse" {
		t.Fatalf("expected system extracted, got %v", user["system"])
	}

	// Continuation: trailing tool message => tool_results with the toolCallId preserved.
	cont := composerInput([]byte(`{"messages":[{"role":"user","content":"q"},{"role":"assistant","tool_calls":[{"id":"tc_1"}]},{"role":"tool","tool_call_id":"tc_1","content":"RESULT"}]}`))
	if cont["type"] != "tool_results" {
		t.Fatalf("expected tool_results turn, got %v", cont["type"])
	}
	results, _ := cont["results"].([]map[string]any)
	if len(results) != 1 || results[0]["toolCallId"] != "tc_1" || results[0]["content"] != "RESULT" {
		t.Fatalf("tool_results not extracted correctly: %#v", cont["results"])
	}
}

func TestComposerInputHistoryAndImages(t *testing.T) {
	// Multi-turn first contact: prior turns rendered as history; last user is the new text.
	in := composerInput([]byte(`{"messages":[
		{"role":"system","content":"S"},
		{"role":"user","content":"first q"},
		{"role":"assistant","content":"first a"},
		{"role":"user","content":[{"type":"text","text":"second q"},{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD"}}]}
	]}`))
	if in["text"] != "second q" {
		t.Fatalf("expected last user text, got %v", in["text"])
	}
	hist, _ := in["history"].(string)
	if !strings.Contains(hist, "USER: first q") || !strings.Contains(hist, "ASSISTANT: first a") {
		t.Fatalf("history not rendered: %q", hist)
	}
	imgs, _ := in["images"].([]map[string]any)
	if len(imgs) != 1 || imgs[0]["data"] != "QUJD" || imgs[0]["mimeType"] != "image/png" {
		t.Fatalf("image not extracted: %#v", in["images"])
	}
}

func TestExtractComposerToolChoice(t *testing.T) {
	cases := map[string]string{
		`{"tool_choice":"none"}`:                                      "none",
		`{"tool_choice":"auto"}`:                                      "auto",
		`{"tool_choice":"required"}`:                                  "required",
		`{"tool_choice":{"type":"function","function":{"name":"X"}}}`: "specific:X",
		`{}`: "",
	}
	for in, want := range cases {
		if got := extractComposerToolChoice([]byte(in)); got != want {
			t.Fatalf("toolChoice(%s)=%q want %q", in, got, want)
		}
	}
}

func TestComposerTurnBody(t *testing.T) {
	// tool_choice=none path: caller passes advertise=nil => no "tools" key on the wire.
	none := composerTurnBody("s1", "composer-2.5", map[string]any{"type": "user", "text": "x"}, nil, "none", nil, nil)
	if gjson.GetBytes(none, "tools").Exists() {
		t.Fatalf("tool_choice=none must not advertise tools: %s", none)
	}
	if gjson.GetBytes(none, "toolChoice").String() != "none" {
		t.Fatalf("toolChoice not forwarded: %s", none)
	}
	// With advertised tools + clientEnv + enforced constraints.
	adv := []map[string]any{{"name": "Read", "toolName": "Read"}}
	full := composerTurnBody("s1", "composer-2.5", map[string]any{"type": "user", "text": "x"}, adv, "auto", map[string]any{"shell": "zsh"},
		map[string]any{"responseFormat": map[string]any{"type": "json_object"}, "stop": []string{"STOP"}, "maxTokens": 256})
	if gjson.GetBytes(full, "tools.0.name").String() != "Read" {
		t.Fatalf("advertised tool missing: %s", full)
	}
	if gjson.GetBytes(full, "clientEnv.shell").String() != "zsh" {
		t.Fatalf("clientEnv not forwarded: %s", full)
	}
	if gjson.GetBytes(full, "responseFormat.type").String() != "json_object" {
		t.Fatalf("responseFormat not forwarded: %s", full)
	}
	if gjson.GetBytes(full, "stop.0").String() != "STOP" || gjson.GetBytes(full, "maxTokens").Int() != 256 {
		t.Fatalf("stop/maxTokens not forwarded: %s", full)
	}
}

// Comment 2: a data-URI image sent through cursor_composer.go must reach the sidecar in the SDK image
// shape {data, mimeType} — the MIME type must survive the Go->bridge translation.
func TestComposerImageCarriesMimeType(t *testing.T) {
	oai := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,SkZJRg=="}}]}]}`)
	body := composerTurnBody("s1", "composer-2.5", composerInput(oai), nil, "", nil, composerConstraints(oai))
	if got := gjson.GetBytes(body, "input.images.0.data").String(); got != "SkZJRg==" {
		t.Fatalf("image data not on the wire: %s", body)
	}
	if got := gjson.GetBytes(body, "input.images.0.mimeType").String(); got != "image/jpeg" {
		t.Fatalf("image mimeType not on the wire (the SDK requires it): %s", body)
	}
}

// Comment 3: response constraints and tool_choice modes become explicit bridge fields.
func TestComposerConstraintsExtraction(t *testing.T) {
	jsonObj := composerConstraints([]byte(`{"response_format":{"type":"json_object"}}`))
	if rf, _ := jsonObj["responseFormat"].(map[string]any); rf["type"] != "json_object" {
		t.Fatalf("json_object response_format not extracted: %#v", jsonObj)
	}
	schema := composerConstraints([]byte(`{"response_format":{"type":"json_schema","json_schema":{"name":"x","schema":{"type":"object"}}}}`))
	if rf, _ := schema["responseFormat"].(map[string]any); rf["type"] != "json_schema" {
		t.Fatalf("json_schema response_format not extracted: %#v", schema)
	}
	// stop as a bare string and as an array.
	if got := extractComposerStop([]byte(`{"stop":"END"}`)); len(got) != 1 || got[0] != "END" {
		t.Fatalf("string stop not extracted: %#v", got)
	}
	if got := extractComposerStop([]byte(`{"stop":["A","B"]}`)); len(got) != 2 {
		t.Fatalf("array stop not extracted: %#v", got)
	}
	// token limit from either field name.
	if extractComposerMaxTokens([]byte(`{"max_tokens":128}`)) != 128 {
		t.Fatalf("max_tokens not extracted")
	}
	if extractComposerMaxTokens([]byte(`{"max_completion_tokens":64}`)) != 64 {
		t.Fatalf("max_completion_tokens not extracted")
	}
	// tool_choice modes.
	if extractComposerToolChoice([]byte(`{"tool_choice":"required"}`)) != "required" {
		t.Fatalf("required tool_choice")
	}
	if extractComposerToolChoice([]byte(`{"tool_choice":{"type":"function","function":{"name":"Bash"}}}`)) != "specific:Bash" {
		t.Fatalf("specific tool_choice")
	}
}

// Legacy OpenAI functions[]/function_call clients must still get their tools advertised + tool_choice
// honored on the default Cursor Composer Client-Tools path (parity with the old Cursor parser).
func TestComposerLegacyFunctions(t *testing.T) {
	oai := []byte(`{"functions":[{"name":"get_weather","description":"w","parameters":{"type":"object"}}],"function_call":{"name":"get_weather"}}`)
	adv := composerAdvertise(oai)
	if len(adv) != 1 || adv[0]["name"] != "get_weather" {
		t.Fatalf("legacy functions[] not advertised: %#v", adv)
	}
	if _, ok := adv[0]["inputSchema"]; !ok {
		t.Fatalf("legacy function parameters not mapped to inputSchema: %#v", adv[0])
	}
	if defs := composerToolDefs(oai); len(defs) != 1 || defs[0].Name != "get_weather" {
		t.Fatalf("legacy functions[] not in tool defs: %#v", defs)
	}
	if got := extractComposerToolChoice(oai); got != "specific:get_weather" {
		t.Fatalf("legacy function_call not honored: %q", got)
	}
	// Modern tools[] takes precedence when both are present.
	both := []byte(`{"tools":[{"type":"function","function":{"name":"modern"}}],"functions":[{"name":"legacy"}]}`)
	if a := composerAdvertise(both); len(a) != 1 || a[0]["name"] != "modern" {
		t.Fatalf("modern tools[] must take precedence: %#v", a)
	}
	if got := extractComposerToolChoice([]byte(`{"function_call":"none"}`)); got != "none" {
		t.Fatalf("legacy function_call string form: %q", got)
	}
}

// L2: a forced tool_choice expressed via an aliased/generic name must resolve to the client's actual tool.
func TestResolveComposerToolChoice(t *testing.T) {
	defs := []cursorToolDefinition{{Name: "RunCommand"}, {Name: "Read"}}
	shell := []byte(`{"tool_choice":{"type":"function","function":{"name":"shell"}}}`)
	if got := resolveComposerToolChoice(shell, defs, nil); got != "specific:shell" {
		t.Fatalf("no match must pass the raw name through, got %q", got)
	}
	if got := resolveComposerToolChoice(shell, defs, map[string]string{"shell": "RunCommand"}); got != "specific:RunCommand" {
		t.Fatalf("aliased specific name not resolved to client tool, got %q", got)
	}
	if got := resolveComposerToolChoice([]byte(`{"tool_choice":{"type":"function","function":{"name":"Read"}}}`), defs, nil); got != "specific:Read" {
		t.Fatalf("exact name should resolve to itself, got %q", got)
	}
	if got := resolveComposerToolChoice([]byte(`{"tool_choice":"none"}`), defs, nil); got != "none" {
		t.Fatalf("none must be untouched, got %q", got)
	}
}

// L3: pins the CRITICAL single-tenant invariant + multi-tenant header wiring of postAgentTurn.
func TestPostAgentTurnBridgeAuthHeader(t *testing.T) {
	var gotAuth, gotBridge string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotBridge = r.Header.Get("X-Bridge-Auth")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()
	e := NewCursorExecutor(&config.Config{})
	mk := func(extra map[string]string) *cliproxyauth.Auth {
		a := map[string]string{"composer_client_tools_bridge_url": srv.URL}
		for k, v := range extra {
			a[k] = v
		}
		return &cliproxyauth.Auth{ID: "t", Attributes: a}
	}
	call := func(auth *cliproxyauth.Auth) {
		resp, err := e.postAgentTurn(context.Background(), auth, "CURSORKEY", []byte("{}"))
		if err != nil {
			t.Fatalf("postAgentTurn: %v", err)
		}
		_ = resp.Body.Close()
	}
	// (a) single-tenant default: NO X-Bridge-Auth; bearer is the cursor key (byte-for-byte backward-compat).
	call(mk(nil))
	if gotBridge != "" {
		t.Fatalf("single-tenant must NOT send X-Bridge-Auth, got %q", gotBridge)
	}
	if gotAuth != "Bearer CURSORKEY" {
		t.Fatalf("bearer must be the cursor key, got %q", gotAuth)
	}
	// (b) per-key attr token => X-Bridge-Auth=attr, bearer still the cursor key.
	call(mk(map[string]string{"composer_client_tools_bridge_token": "ATTRTOK"}))
	if gotBridge != "ATTRTOK" || gotAuth != "Bearer CURSORKEY" {
		t.Fatalf("attr token: bridge=%q auth=%q", gotBridge, gotAuth)
	}
	// (c) env token (no attr).
	t.Setenv("CURSOR_AGENT_BRIDGE_TOKEN", "ENVTOK")
	call(mk(nil))
	if gotBridge != "ENVTOK" {
		t.Fatalf("env token not forwarded, got %q", gotBridge)
	}
	// (d) attr wins over env.
	call(mk(map[string]string{"composer_client_tools_bridge_token": "ATTRWINS"}))
	if gotBridge != "ATTRWINS" {
		t.Fatalf("attr must win over env, got %q", gotBridge)
	}
}

func TestMapComposerToolCall(t *testing.T) {
	defs := []cursorToolDefinition{{Name: "Read"}, {Name: "Bash"}}
	// generic native name maps to the client's exact tool via resolveToolSpec.
	name, args := mapComposerToolCall("read", gjson.Parse(`{"path":"/x"}`), defs, nil)
	if name != "Read" {
		t.Fatalf("expected read->Read, got %s", name)
	}
	if !gjson.Valid(args) {
		t.Fatalf("args not valid JSON: %s", args)
	}
	// no match against the advertised set => pass through (the bridge handles unknowns).
	n2, _ := mapComposerToolCall("totally_unknown_tool", gjson.Parse(`{}`), defs, nil)
	if n2 != "totally_unknown_tool" {
		t.Fatalf("unmatched name should pass through, got %s", n2)
	}
}

// #47: configurable tool-alias overrides (env + YAML) win over the built-in table and route a
// Cursor-emitted/generic name to the client's actual tool.
func TestComposerToolAliasOverrides(t *testing.T) {
	defs := []cursorToolDefinition{{Name: "RunCommand"}, {Name: "Read"}}
	// Without an override, "shell" resolves via the built-in table to Bash — absent here => pass-through.
	if n, _ := mapComposerToolCall("shell", gjson.Parse(`{}`), defs, nil); n != "shell" {
		t.Fatalf("no override + no Bash tool should pass through, got %s", n)
	}
	// With an override shell->RunCommand, it routes to the client's RunCommand tool.
	ov := map[string]string{"shell": "RunCommand"}
	if n, _ := mapComposerToolCall("shell", gjson.Parse(`{"command":"ls"}`), defs, ov); n != "RunCommand" {
		t.Fatalf("override shell->RunCommand not applied, got %s", n)
	}
	// Override wins over the built-in exact/normalized match (conflict resolution).
	ov2 := map[string]string{"read": "RunCommand"}
	if n, _ := mapComposerToolCall("read", gjson.Parse(`{}`), defs, ov2); n != "RunCommand" {
		t.Fatalf("override should win on conflict, got %s", n)
	}

	// composerToolAliases merges env (base) + per-key attr (wins); parses JSON and from=to forms.
	t.Setenv("CURSOR_TOOL_ALIASES", "shell=EnvShell,grep=EnvGrep")
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"tool_aliases": `{"shell":"KeyShell"}`}}
	got := composerToolAliases(auth)
	if got["shell"] != "KeyShell" {
		t.Fatalf("per-key alias must win over env, got %q", got["shell"])
	}
	if got["grep"] != "EnvGrep" {
		t.Fatalf("env alias must apply when not overridden, got %q", got["grep"])
	}
}

// LOW#4: one non-string value in the JSON alias blob must not discard the whole map — valid entries survive.
func TestComposerToolAliasesMalformed(t *testing.T) {
	t.Setenv("CURSOR_TOOL_ALIASES", `{"shell":"RunCommand","grep":123,"ls":"List"}`)
	got := composerToolAliases(nil)
	if got["shell"] != "RunCommand" || got["ls"] != "List" {
		t.Fatalf("valid aliases dropped due to a sibling non-string value: %#v", got)
	}
	if _, ok := got["grep"]; ok {
		t.Fatalf("non-string alias value should be skipped, got %v", got["grep"])
	}
}

// LOW#3: history re-seeding must preserve assistant tool-call INTENT (name+args) and tag tool results with
// their tool_call_id, so a stateless multi-turn client doesn't lose "what you just called" context.
func TestRenderComposerHistoryPreservesToolCalls(t *testing.T) {
	messages := gjson.GetBytes([]byte(`{"messages":[
		{"role":"user","content":"read foo.go"},
		{"role":"assistant","content":"","tool_calls":[{"id":"tc_1","function":{"name":"Read","arguments":"{\"path\":\"foo.go\"}"}}]},
		{"role":"tool","tool_call_id":"tc_1","content":"package main"},
		{"role":"assistant","content":"done reading"},
		{"role":"user","content":"now edit it"}
	]}`), "messages").Array()
	h := renderComposerHistory(messages, 4) // history = messages[0..3]
	if !strings.Contains(h, "tool_calls: tc_1:Read(") {
		t.Fatalf("assistant tool-call intent dropped from history: %q", h)
	}
	if !strings.Contains(h, "TOOL[tc_1]: package main") {
		t.Fatalf("tool result not tagged with its tool_call_id: %q", h)
	}
	if !strings.Contains(h, "ASSISTANT: done reading") {
		t.Fatalf("assistant text turn missing from history: %q", h)
	}
}

func TestOaiChunkShape(t *testing.T) {
	line := oaiChunk("chatcmpl-composer-deadbeef", "composer-2.5", map[string]any{"index": 0, "delta": map[string]any{"content": "hi"}})
	if !strings.HasPrefix(string(line), "data: ") {
		t.Fatalf("chunk must be an SSE data line: %q", line)
	}
	body := strings.TrimPrefix(string(line), "data: ")
	if gjson.Get(body, "object").String() != "chat.completion.chunk" {
		t.Fatalf("bad chunk object: %s", body)
	}
	if gjson.Get(body, "id").String() != "chatcmpl-composer-deadbeef" {
		t.Fatalf("chunk id not propagated: %s", body)
	}
	if gjson.Get(body, "choices.0.delta.content").String() != "hi" {
		t.Fatalf("content delta missing: %s", body)
	}
}

// Comment 2: the non-streaming path must surface text, reasoning, tool calls, and usage, and propagate
// a bridge error. Drives executeComposer end-to-end against a mock /agent/turn SSE bridge.
func TestExecuteComposerNonStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl, _ := w.(http.Flusher)
		write := func(s string) {
			_, _ = w.Write([]byte("data: " + s + "\n\n"))
			if fl != nil {
				fl.Flush()
			}
		}
		if gjson.GetBytes(body, "input.text").String() == "FAIL" {
			write(`{"type":"turn_end","stop_reason":"error","error":"cursor quota exceeded"}`)
			write("[DONE]")
			return
		}
		if gjson.GetBytes(body, "input.text").String() == "BADUSAGE" {
			write(`{"type":"text","delta":"partial output"}`)
			write(`{"type":"turn_end","stop_reason":"stop","usage":{"prompt_tokens":11`) // truncated usage VALUE
			write("[DONE]")
			return
		}
		write(`{"type":"reasoning","delta":"thinking..."}`)
		write(`{"type":"text","delta":"Hello "}`)
		write(`{"type":"text","delta":"world"}`)
		write(`{"type":"tool_call","id":"tc_1","name":"read","input":{"path":"/x"}}`)
		write(`{"type":"turn_end","stop_reason":"tool_use","usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`)
		write("[DONE]")
	}))
	defer srv.Close()

	e := NewCursorExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{ID: "authT", Attributes: map[string]string{"api_key": "k", "composer_client_tools_bridge_url": srv.URL}}
	mkOpts := func() cliproxyexecutor.Options {
		return cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai"), Headers: http.Header{"X-Conversation-Id": []string{"conv-nonstream"}}}
	}
	mkReq := func(text string) cliproxyexecutor.Request {
		payload := []byte(`{"model":"composer-2.5","messages":[{"role":"user","content":"` + text + `"}],"tools":[{"type":"function","function":{"name":"Read","description":"read a file","parameters":{"type":"object"}}}]}`)
		return cliproxyexecutor.Request{Model: "composer-2.5", Payload: payload}
	}

	resp, err := e.executeComposer(context.Background(), auth, "k", mkReq("hi"), mkOpts())
	if err != nil {
		t.Fatalf("executeComposer error: %v", err)
	}
	body := string(resp.Payload)
	if !strings.Contains(gjson.Get(body, "choices.0.message.content").String(), "Hello world") {
		t.Fatalf("text not accumulated: %s", body)
	}
	if !strings.Contains(gjson.Get(body, "choices.0.message.reasoning_content").String(), "thinking") {
		t.Fatalf("reasoning_content dropped from non-stream response: %s", body)
	}
	if gjson.Get(body, "choices.0.message.tool_calls.0.function.name").String() != "Read" {
		t.Fatalf("tool_call name (mapped read->Read) missing: %s", body)
	}
	if gjson.Get(body, "usage.total_tokens").Int() != 18 {
		t.Fatalf("usage dropped from non-stream response: %s", body)
	}

	// Bridge error must propagate as a Go error, not a masked empty success.
	if _, errFail := e.executeComposer(context.Background(), auth, "k", mkReq("FAIL"), mkOpts()); errFail == nil || !strings.Contains(errFail.Error(), "cursor quota exceeded") {
		t.Fatalf("bridge error must propagate, got: %v", errFail)
	}

	// M1: a malformed turn_end.usage value must NOT corrupt the whole response into an empty 200 (data
	// loss). The accumulated text must survive and the bad usage must simply be omitted.
	respBad, errBad := e.executeComposer(context.Background(), auth, "k", mkReq("BADUSAGE"), mkOpts())
	if errBad != nil {
		t.Fatalf("malformed usage must not error the turn: %v", errBad)
	}
	if len(respBad.Payload) == 0 {
		t.Fatalf("malformed usage produced an empty response body (data loss)")
	}
	bodyBad := string(respBad.Payload)
	if !strings.Contains(gjson.Get(bodyBad, "choices.0.message.content").String(), "partial output") {
		t.Fatalf("assistant text lost on malformed usage: %s", bodyBad)
	}
	if gjson.Get(bodyBad, "usage").Exists() {
		t.Fatalf("malformed usage must be omitted, not spliced into the body: %s", bodyBad)
	}
}

// The bridge's typed {"type":"ping"} keepalive must reach the client as a real, schema-correct keepalive
// frame (NOT the dropped ": keepalive" comment), so a long or QUEUED turn never trips a client idle-abort.
// Rendering is keyed on the INBOUND schema, never client identity: Anthropic gets a typed ping AFTER
// message_start; OpenAI gets benign empty-delta no-op chunks.
func TestExecuteComposerStreamPingKeepalive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl, _ := w.(http.Flusher)
		write := func(s string) {
			_, _ = w.Write([]byte("data: " + s + "\n\n"))
			if fl != nil {
				fl.Flush()
			}
		}
		write(`{"type":"ping"}`) // before any content (the queued-wait / slow-model case)
		write(`{"type":"ping"}`)
		write(`{"type":"text","delta":"hi"}`)
		write(`{"type":"turn_end","stop_reason":"stop"}`)
		write("[DONE]")
	}))
	defer srv.Close()
	e := NewCursorExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{ID: "authP", Attributes: map[string]string{"api_key": "k", "composer_client_tools_bridge_url": srv.URL}}
	collect := func(sr *cliproxyexecutor.StreamResult) string {
		var b strings.Builder
		for chunk := range sr.Chunks {
			if chunk.Err == nil {
				b.Write(chunk.Payload)
			}
		}
		return b.String()
	}

	// Anthropic client: the keepalive must reach the client as a real, typed `ping` event (the whole point —
	// the bridge's ": keepalive" comment is dropped before the client). The message envelope must open before
	// the typed ping (an empty delta lazily emits message_start), so the ping is never the first frame.
	claudeReq := cliproxyexecutor.Request{Model: "composer-2.5", Payload: []byte(`{"model":"composer-2.5","messages":[{"role":"user","content":"hi"}],"tools":[{"name":"Read","input_schema":{"type":"object"}}]}`)}
	srA, err := e.executeComposerStream(context.Background(), auth, "k", claudeReq,
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude"), Headers: http.Header{"X-Conversation-Id": []string{"ping-a"}}})
	if err != nil {
		t.Fatalf("stream(claude): %v", err)
	}
	outA := collect(srA)
	if !strings.Contains(outA, "event: ping") {
		t.Fatalf("anthropic keepalive ping not rendered to the client (it would have been silently dropped): %q", outA)
	}
	// The message envelope (an Anthropic message frame) must precede the first typed ping.
	if i, j := strings.Index(outA, `"type":"message"`), strings.Index(outA, "event: ping"); i < 0 || i > j {
		t.Fatalf("typed ping must NOT precede the message envelope (msg@%d ping@%d): %q", i, j, outA)
	}
	// The stream MUST close with a terminal (message_stop) — the bridge's [DONE] is consumed by the executor,
	// so it synthesizes one through the translator. Without it the Anthropic SDK hangs waiting to finalize.
	if !strings.Contains(outA, "message_stop") {
		t.Fatalf("anthropic stream missing terminal message_stop (client would hang): %q", outA)
	}

	// OpenAI client: the ping renders as a benign no-op chunk (never a bare comment), and real content survives.
	oaiReq := cliproxyexecutor.Request{Model: "composer-2.5", Payload: []byte(`{"model":"composer-2.5","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"Read","parameters":{"type":"object"}}}]}`)}
	srO, err := e.executeComposerStream(context.Background(), auth, "k", oaiReq,
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai"), Headers: http.Header{"X-Conversation-Id": []string{"ping-o"}}})
	if err != nil {
		t.Fatalf("stream(openai): %v", err)
	}
	outO := collect(srO)
	if !strings.Contains(outO, "chat.completion.chunk") {
		t.Fatalf("openai stream missing chunks: %q", outO)
	}
	if !strings.Contains(outO, "hi") {
		t.Fatalf("openai content lost (ping handling must not drop real content): %q", outO)
	}
}

// L1: callerCredential must cover all inbound credential forms (headers + query params) so distinct
// callers sharing one Cursor key are isolated regardless of how they authenticate.
func TestCallerCredentialForms(t *testing.T) {
	if got := callerCredential(optsWithHeaders(map[string]string{"X-Goog-Api-Key": "gkey"})); got != "gkey" {
		t.Fatalf("X-Goog-Api-Key not folded: %q", got)
	}
	if got := callerCredential(cliproxyexecutor.Options{Query: url.Values{"key": []string{"qkey"}}}); got != "qkey" {
		t.Fatalf("?key= not folded: %q", got)
	}
	if got := callerCredential(cliproxyexecutor.Options{Query: url.Values{"auth_token": []string{"atok"}}}); got != "atok" {
		t.Fatalf("?auth_token= not folded: %q", got)
	}
	// Distinct X-Goog-Api-Key callers sharing one Cursor key + conversation id get distinct sessions.
	auth := authWith("authA", "keyA")
	g1, _ := deriveComposerSessionID(auth, toolTurn("x"), optsWithHeaders(map[string]string{"X-Conversation-Id": "shared", "X-Goog-Api-Key": "user-one"}))
	g2, _ := deriveComposerSessionID(auth, toolTurn("x"), optsWithHeaders(map[string]string{"X-Conversation-Id": "shared", "X-Goog-Api-Key": "user-two"}))
	if g1 == g2 {
		t.Fatalf("distinct X-Goog-Api-Key callers must get distinct sessions: %s == %s", g1, g2)
	}
}

// Per-request response id must be unique so clients never conflate unrelated responses.
func TestComposerResponseIDUnique(t *testing.T) {
	a, b := composerResponseID(), composerResponseID()
	if a == b {
		t.Fatalf("response ids must differ per request: %s == %s", a, b)
	}
	if !strings.HasPrefix(a, "chatcmpl-composer-") {
		t.Fatalf("unexpected response id prefix: %s", a)
	}
}

func TestExtractComposerClientEnv(t *testing.T) {
	env := extractComposerClientEnv(optsWithHeaders(map[string]string{
		"X-Workspace-Path": "/home/u/proj", "X-Cwd": "/home/u/proj/sub", "X-Shell": "fish", "X-Os-Version": "linux 6.1",
	}))
	b, _ := json.Marshal(env)
	if gjson.GetBytes(b, "shell").String() != "fish" || gjson.GetBytes(b, "processWorkingDirectory").String() != "/home/u/proj/sub" {
		t.Fatalf("clientEnv not parsed from headers: %s", b)
	}
	if gjson.GetBytes(b, "workspacePaths.0").String() != "/home/u/proj" {
		t.Fatalf("workspacePaths not parsed: %s", b)
	}
	if extractComposerClientEnv(cliproxyexecutor.Options{}) != nil {
		t.Fatalf("no headers => nil clientEnv")
	}
}

func TestCursorDirectEnabled(t *testing.T) {
	t.Setenv("CURSOR_DIRECT", "1")
	if !cursorDirectEnabled() {
		t.Fatalf("CURSOR_DIRECT=1 should enable direct")
	}
	t.Setenv("CURSOR_DIRECT", "0")
	if cursorDirectEnabled() {
		t.Fatalf("CURSOR_DIRECT=0 should not enable direct")
	}
}
