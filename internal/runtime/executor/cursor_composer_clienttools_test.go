package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

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
	a, err := deriveComposerSessionID(authWith("authA", "keyA"), "cursorkey", toolTurn("hello"), convoOpts("conv-1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(a, "sess_") {
		t.Fatalf("session id should be prefixed sess_, got %s", a)
	}
	a2, _ := deriveComposerSessionID(authWith("authA", "keyA"), "cursorkey", toolTurn("different text"), convoOpts("conv-1"))
	if a != a2 {
		t.Fatalf("same conversation id must be stable regardless of content: %s vs %s", a, a2)
	}
	// Different tenant, same conversation id => different id (no cross-tenant collision).
	b, _ := deriveComposerSessionID(authWith("authB", "keyB"), "cursorkey", toolTurn("hello"), convoOpts("conv-1"))
	if a == b {
		t.Fatalf("different tenants must not share a session id")
	}
	// Different conversation id => different id.
	c, _ := deriveComposerSessionID(authWith("authA", "keyA"), "cursorkey", toolTurn("hello"), convoOpts("conv-2"))
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
	o1, _ := deriveComposerSessionID(auth, "cursorkey", title, conv)
	o2, _ := deriveComposerSessionID(auth, "cursorkey", title, conv)
	o3, _ := deriveComposerSessionID(auth, "cursorkey", summary, conv)
	if o1 == "" || o2 == "" || o3 == "" {
		t.Fatalf("utility one-shot must still route to a session (o1=%q o2=%q o3=%q)", o1, o2, o3)
	}
	if o1 == o2 || o1 == o3 || o2 == o3 {
		t.Fatalf("utility one-shots on the same conv must each get a distinct ephemeral session: %s %s %s", o1, o2, o3)
	}

	// The real agentic turn (advertises tools) on that SAME conversation id keeps the stable session, and
	// it must not collide with the isolated one-shots.
	r1, _ := deriveComposerSessionID(auth, "cursorkey", toolTurn("describe this repo"), conv)
	r2, _ := deriveComposerSessionID(auth, "cursorkey", toolTurn("describe this repo"), conv)
	if r1 != r2 {
		t.Fatalf("agentic turns on the same conv must share the stable session: %s vs %s", r1, r2)
	}
	if r1 == o1 || r1 == o2 || r1 == o3 {
		t.Fatalf("utility one-shot must not collide with the conversation's stable session")
	}

	// A continuation (last message is a tool result) is NOT a one-shot even with no tools advertised:
	// it must still route to the conversation's stable session (here via the stable conversation id).
	cont := []byte(`{"messages":[{"role":"assistant","tool_calls":[{"id":"tc_1"}]},{"role":"tool","tool_call_id":"tc_1","content":"R"}]}`)
	c1, err := deriveComposerSessionID(auth, "cursorkey", cont, conv)
	if err != nil {
		t.Fatalf("continuation must route, not error: %v", err)
	}
	if c1 != r1 {
		t.Fatalf("continuation must route to the stable session (got %s want %s)", c1, r1)
	}
}

// A stateless new-user turn (no conversation id, no metadata.user_id — curl/SDK/simple UI) must NOT be
// rejected: it gets a fresh RANDOM session id (collision-free, unlike a content hash). EX6: an unroutable
// continuation also mints (degrade-gracefully), so no routine turn errors. A stable metadata.user_id still
// routes deterministically and is content-independent.
func TestDeriveComposerSessionID_StatelessMint(t *testing.T) {
	auth := authWith("authA", "keyA")
	a, err := deriveComposerSessionID(auth, "cursorkey", []byte(`{"messages":[{"role":"user","content":"hello"}]}`), cliproxyexecutor.Options{})
	if err != nil || !strings.HasPrefix(a, "sess_") {
		t.Fatalf("stateless new turn must mint a session, not error (id=%q err=%v)", a, err)
	}
	// Two distinct stateless conversations must NOT collide (fresh random per new turn).
	b, _ := deriveComposerSessionID(auth, "cursorkey", []byte(`{"messages":[{"role":"user","content":"hello"}]}`), cliproxyexecutor.Options{})
	if a == b {
		t.Fatalf("distinct stateless new turns must get distinct minted ids")
	}
	// metadata.user_id (Claude session) routes an agentic turn deterministically and is content-independent.
	o := cliproxyexecutor.Options{OriginalRequest: []byte(`{"metadata":{"user_id":"user_x_account__session_uuid-1"}}`)}
	s1, err := deriveComposerSessionID(auth, "cursorkey", toolTurn("hi"), o)
	if err != nil || s1 == "" {
		t.Fatalf("metadata.user_id should route: %v", err)
	}
	s1b, _ := deriveComposerSessionID(auth, "cursorkey", toolTurn("different"), o)
	if s1 != s1b {
		t.Fatalf("same Claude session uuid must be stable regardless of content")
	}
	s2, _ := deriveComposerSessionID(auth, "cursorkey", toolTurn("hi"), cliproxyexecutor.Options{OriginalRequest: []byte(`{"metadata":{"user_id":"user_x_account__session_uuid-2"}}`)})
	if s1 == s2 {
		t.Fatalf("different Claude session uuids must differ")
	}
}

// Comment 1: a continuation (tool_results) turn with no stable id routes by a previously emitted
// tool_call_id. EX6: an unknown tool_call_id with no stable id no longer errors — it DEGRADES GRACEFULLY by
// minting a fresh session (the continuation carries history+system, so the bridge re-seeds).
func TestDeriveComposerSessionID_ToolCallContinuation(t *testing.T) {
	auth := authWith("authA", "keyA")
	sid, err := deriveComposerSessionID(auth, "cursorkey", toolTurn("start"), optsWithHeaders(map[string]string{"X-Conversation-Id": "conv-x"}))
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	recordComposerToolCall(composerTenant(auth, cliproxyexecutor.Options{}), "tc_42", sid)
	cont := []byte(`{"messages":[{"role":"assistant","tool_calls":[{"id":"tc_42"}]},{"role":"tool","tool_call_id":"tc_42","content":"R"}]}`)
	got, err := deriveComposerSessionID(auth, "cursorkey", cont, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("continuation should route by tool_call_id: %v", err)
	}
	if got != sid {
		t.Fatalf("continuation routed to wrong session: %s vs %s", got, sid)
	}
	// EX6: an unknown tool_call_id with no stable id MINTS a fresh session (degrade-gracefully, never 500).
	unknown := []byte(`{"messages":[{"role":"tool","tool_call_id":"tc_unknown","content":"R"}]}`)
	gotU, errU := deriveComposerSessionID(auth, "cursorkey", unknown, cliproxyexecutor.Options{})
	if errU != nil {
		t.Fatalf("unknown tool_call_id with no stable id must NOT error (EX6 degrade-gracefully): %v", errU)
	}
	if !strings.HasPrefix(gotU, "sess_") {
		t.Fatalf("unknown continuation must mint a fresh sess_ id, got %q", gotU)
	}
	// L1: the SAME tool_call_id under a DIFFERENT tenant must NOT resolve to the first tenant's session. With
	// EX6 it no longer errors; it mints a fresh session DISTINCT from the first tenant's emitting session.
	gotB, errB := deriveComposerSessionID(authWith("authB", "keyB"), "cursorkey", cont, cliproxyexecutor.Options{})
	if errB != nil {
		t.Fatalf("cross-tenant continuation must not error (EX6): %v", errB)
	}
	if gotB == sid {
		t.Fatalf("cross-tenant tool_call_id reuse must NOT resolve to tenant A's session (got %s)", gotB)
	}
}

// Same conversation id but different inbound caller credential => different sessions (separates users
// who share one upstream Cursor key).
func TestDeriveComposerSessionID_CallerIsolation(t *testing.T) {
	auth := authWith("authA", "keyA")
	u1, _ := deriveComposerSessionID(auth, "cursorkey", toolTurn("x"), optsWithHeaders(map[string]string{"X-Conversation-Id": "shared", "Authorization": "Bearer userone"}))
	u2, _ := deriveComposerSessionID(auth, "cursorkey", toolTurn("x"), optsWithHeaders(map[string]string{"X-Conversation-Id": "shared", "Authorization": "Bearer usertwo"}))
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

// RC3: a client-facing model alias resolves to its configured upstream Cursor model name (via the auth's
// model_aliases attribute) before the request is sent; unknown names pass through; normalization still applies.
func TestResolveCursorModelAlias(t *testing.T) {
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"model_aliases": `{"my-fast":"composer-2.5-fast","big":"composer-2.5"}`}}
	if got := resolveCursorModelAlias(auth, "my-fast"); got != "composer-2.5-fast" {
		t.Fatalf("alias my-fast -> %q, want composer-2.5-fast", got)
	}
	if got := resolveCursorModelAlias(auth, "big"); got != "composer-2.5" {
		t.Fatalf("alias big -> %q, want composer-2.5", got)
	}
	if got := resolveCursorModelAlias(auth, "composer-2.5"); got != "composer-2.5" {
		t.Fatalf("unmapped name must pass through, got %q", got)
	}
	if got := resolveCursorModelAlias(&cliproxyauth.Auth{}, "anything"); got != "anything" {
		t.Fatalf("no aliases configured -> passthrough, got %q", got)
	}
	// End-to-end with normalization: alias -> upstream -> normalized canonical id.
	if got := resolveCursorModelName(resolveCursorModelAlias(auth, "my-fast")); got != "composer-2.5-fast" {
		t.Fatalf("alias+normalize -> %q, want composer-2.5-fast", got)
	}
}

// A mixed tool_results+text turn (Claude Code bundles tool_results AND a new user message when you interrupt
// a tool or background a task and then type) must NEVER error — erroring 500s a routine client turn. Instead
// the trailing user text is FOLDED into the last tool result's content so the model answers both in one turn.
// A pure tool_results turn (no trailing text) is left exactly as-is.
func TestComposerMixedTurnFoldsTrailingText(t *testing.T) {
	mixed := composerInput([]byte(`{"messages":[{"role":"assistant","content":"","tool_calls":[{"id":"tc_1","function":{"name":"Read"}}]},{"role":"tool","tool_call_id":"tc_1","content":"R"},{"role":"user","content":"also do X"}]}`))
	if mixed["type"] != "tool_results" {
		t.Fatalf("mixed turn must classify as tool_results, got %v", mixed["type"])
	}
	results, _ := mixed["results"].([]map[string]any)
	if len(results) != 1 {
		t.Fatalf("expected 1 tool result, got %d", len(results))
	}
	if content, _ := results[len(results)-1]["content"].(string); !strings.Contains(content, "R") || !strings.Contains(content, "also do X") {
		t.Fatalf("trailing user text must be folded into the last tool result content (no error), got %q", content)
	}
	if _, hasTrailing := mixed["trailingText"]; hasTrailing {
		t.Fatalf("trailingText must not be exposed as a separate signal once folded, got %v", mixed["trailingText"])
	}
	// A pure tool_results turn (no trailing text) must NOT be modified.
	pure := composerInput([]byte(`{"messages":[{"role":"assistant","content":"","tool_calls":[{"id":"tc_1","function":{"name":"Read"}}]},{"role":"tool","tool_call_id":"tc_1","content":"R"}]}`))
	pres, _ := pure["results"].([]map[string]any)
	if len(pres) != 1 {
		t.Fatalf("pure: expected 1 result, got %d", len(pres))
	}
	if c, _ := pres[0]["content"].(string); c != "R" {
		t.Fatalf("a pure tool_results turn must NOT be modified, got content %q", c)
	}
}

// RC4: re-translating an already-OpenAI payload through the source(claude)->openai translator CORRUPTS it
// (the gated CURSOR_DIRECT path's double-translation bug — here it drops the tool name). The fix reuses the
// single-translation result instead of re-translating; this test pins the hazard.
func TestDirectPathDoubleTranslateCorrupts(t *testing.T) {
	from := sdktranslator.FromString("claude")
	to := sdktranslator.FromString("openai")
	claudeReq := []byte(`{"model":"m","max_tokens":10,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"tools":[{"name":"Read","input_schema":{"type":"object"}}]}`)
	once := sdktranslator.TranslateRequest(from, to, "m", append([]byte(nil), claudeReq...), false)
	twice := sdktranslator.TranslateRequest(from, to, "m", append([]byte(nil), once...), false)
	if gjson.GetBytes(once, "tools.0.function.name").String() != "Read" {
		t.Fatalf("single source->openai translation must preserve the tool name; got %s", once)
	}
	if gjson.GetBytes(twice, "tools.0.function.name").String() == "Read" {
		t.Fatalf("double translation should have CORRUPTED the tool name (the direct path must NOT re-translate); got %s", twice)
	}
}

// RC5: a bridge turn_end{stop_reason:error} (e.g. an unknown/expired session for a tool_results turn) must
// surface to the client as a stream ERROR, never a successful empty turn that hides the lost continuation.
func TestExecuteComposerStreamBridgeErrorSurfaces(t *testing.T) {
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
		write(`{"type":"turn_end","stop_reason":"error","error":"unknown or expired session: continuation cannot be resumed"}`)
		write("[DONE]")
	}))
	defer srv.Close()
	e := NewCursorExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{ID: "t", Attributes: map[string]string{"api_key": "k", "composer_client_tools_bridge_url": srv.URL}}
	req := cliproxyexecutor.Request{Model: "composer-2.5", Payload: []byte(`{"model":"composer-2.5","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"Read"}}]}`)}
	sr, err := e.executeComposerStream(context.Background(), auth, "k", req,
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai"), Headers: http.Header{"X-Conversation-Id": []string{"err1"}}})
	if err != nil {
		t.Fatalf("stream setup: %v", err)
	}
	sawErr := false
	for chunk := range sr.Chunks {
		if chunk.Err != nil {
			sawErr = true
		}
	}
	if !sawErr {
		t.Fatalf("a bridge turn_end{error} must surface as a stream error, not a clean/empty success")
	}
}

// RC1: a loopback bridge call must bypass a configured outbound proxy (don't leak the Cursor bearer to the
// proxy / break localhost routing). With an auth proxy set, a request to a 127.0.0.1 bridge must reach the
// bridge DIRECTLY, never the proxy.
func TestPostAgentTurnLoopbackBypassesProxy(t *testing.T) {
	var bridgeHit, proxyHit bool
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bridgeHit = true
		w.WriteHeader(200)
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer bridge.Close()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHit = true
		w.WriteHeader(200)
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer proxy.Close()
	e := NewCursorExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{ID: "t", ProxyURL: proxy.URL, Attributes: map[string]string{"composer_client_tools_bridge_url": bridge.URL}}
	resp, err := e.postAgentTurn(context.Background(), auth, "CURSORKEY", []byte("{}"))
	if err != nil {
		t.Fatalf("postAgentTurn: %v", err)
	}
	_ = resp.Body.Close()
	if !bridgeHit {
		t.Fatalf("loopback bridge request did not reach the bridge directly")
	}
	if proxyHit {
		t.Fatalf("loopback bridge request was routed through the outbound proxy — must bypass it (credential-leak)")
	}
	// Sanity: the helper detects loopback for the canonical defaults.
	for _, u := range []string{"http://127.0.0.1:9798", "http://localhost:9798/agent/turn", "http://[::1]:9798"} {
		if !isLoopbackBridgeURL(u) {
			t.Fatalf("isLoopbackBridgeURL(%q) = false, want true", u)
		}
	}
	if isLoopbackBridgeURL("https://bridge.example.com/agent/turn") {
		t.Fatalf("remote bridge URL must NOT be treated as loopback")
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
			// M1: a VALID-JSON terminal frame whose usage VALUE is malformed (a string, not a token object).
			// The frame is valid JSON (so the ADD-96 protocol gate accepts it as a real terminal), but
			// ParseOpenAIStreamUsage rejects the usage node — so usage is dropped while the text survives.
			// (A truncated/invalid-JSON FRAME is now a protocol violation by ADD-96/RBT-013, covered separately.)
			write(`{"type":"turn_end","stop_reason":"stop","usage":"oops-not-an-object"}`)
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

// Comment 4 (empirical): a pure Anthropic tool_results request with N parallel tool_result blocks must
// survive TranslateRequest(claude->openai) + composerInput as N results — pins whether of<N desync is
// translation/extraction loss vs. the bridge's batching. Also covers a mixed tool_result+trailing-text turn.
func TestComposerToolResultsExtractionCount(t *testing.T) {
	from := sdktranslator.FromString("claude")
	to := sdktranslator.FromString("openai")
	mk := func(n int, trailingText bool) []byte {
		var tu, tr strings.Builder
		for i := 0; i < n; i++ {
			if i > 0 {
				tu.WriteString(",")
				tr.WriteString(",")
			}
			id := fmt.Sprintf("toolu_%d", i)
			tu.WriteString(fmt.Sprintf(`{"type":"tool_use","id":"%s","name":"Read","input":{"path":"/f%d"}}`, id, i))
			tr.WriteString(fmt.Sprintf(`{"type":"tool_result","tool_use_id":"%s","content":"result %d"}`, id, i))
		}
		if trailingText {
			tr.WriteString(`,{"type":"text","text":"also please continue"}`)
		}
		return []byte(fmt.Sprintf(`{"model":"composer-2.5","messages":[{"role":"user","content":"go"},{"role":"assistant","content":[%s]},{"role":"user","content":[%s]}]}`, tu.String(), tr.String()))
	}
	for _, n := range []int{2, 4} {
		oai := sdktranslator.TranslateRequest(from, to, "composer-2.5", mk(n, false), false)
		inp := composerInput(oai)
		results, _ := inp["results"].([]map[string]any)
		t.Logf("n=%d trailingText=false -> type=%v results=%d oai=%s", n, inp["type"], len(results), string(oai))
		if inp["type"] != "tool_results" || len(results) != n {
			t.Fatalf("n=%d: expected tool_results with %d results, got type=%v count=%d", n, n, inp["type"], len(results))
		}
	}
	// Mixed: tool_results + trailing text must still classify as tool_results (Comment 4), not a fresh user
	// turn, and the trailing text must be FOLDED into the last result (carried, never dropped, never an error).
	oai := sdktranslator.TranslateRequest(from, to, "composer-2.5", mk(2, true), false)
	inp := composerInput(oai)
	results, _ := inp["results"].([]map[string]any)
	if inp["type"] != "tool_results" || len(results) != 2 {
		t.Fatalf("mixed tool_result+text must classify as tool_results with 2 results, got type=%v count=%d", inp["type"], len(results))
	}
	if c, _ := results[len(results)-1]["content"].(string); !strings.Contains(c, "continue") {
		t.Fatalf("mixed turn must fold the trailing user text into the last result content, got %q", c)
	}
}

// Comment 5: a tool_results continuation routes by tool_call_id OWNERSHIP ahead of the stable conv id. When
// stable metadata derives session A but the tool was emitted under session B, the continuation must route to B.
func TestDeriveComposerSessionID_ContinuationOwnershipWins(t *testing.T) {
	auth := authWith("authA", "keyA")
	convA := optsWithHeaders(map[string]string{"X-Conversation-Id": "conv-A"})
	sessionA, _ := deriveComposerSessionID(auth, "cursorkey", toolTurn("x"), convA)
	sessionB := "sess_ownerB000000000000000000000000"
	recordComposerToolCall(composerTenant(auth, convA), "tc_owned_by_B", sessionB)
	// A continuation carrying conv-A metadata (which derives sessionA) but answering a tool emitted by B.
	cont := []byte(`{"messages":[{"role":"assistant","tool_calls":[{"id":"tc_owned_by_B"}]},{"role":"tool","tool_call_id":"tc_owned_by_B","content":"R"}]}`)
	got, err := deriveComposerSessionID(auth, "cursorkey", cont, convA)
	if err != nil {
		t.Fatalf("ownership continuation must route, not error: %v", err)
	}
	if got != sessionB {
		t.Fatalf("continuation must route to the emitting session B (%s), not the stable conv-id session A (%s); got %s", sessionB, sessionA, got)
	}
	// ADD-70: re-emitting the SAME id under session A makes it owned by {A,B} — now AMBIGUOUS, not
	// latest-writer-wins. Because this continuation carries the conv-A stable id, the ambiguity is
	// disambiguated by routing to the conv-A stable session (which is sessionA), never by guessing.
	recordComposerToolCall(composerTenant(auth, convA), "tc_owned_by_B", sessionA)
	if got2, _ := deriveComposerSessionID(auth, "cursorkey", cont, convA); got2 != sessionA {
		t.Fatalf("ambiguous re-emitted id with a stable conv id must route to the conv-A session A (%s), got %s", sessionA, got2)
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
	g1, _ := deriveComposerSessionID(auth, "cursorkey", toolTurn("x"), optsWithHeaders(map[string]string{"X-Conversation-Id": "shared", "X-Goog-Api-Key": "user-one"}))
	g2, _ := deriveComposerSessionID(auth, "cursorkey", toolTurn("x"), optsWithHeaders(map[string]string{"X-Conversation-Id": "shared", "X-Goog-Api-Key": "user-two"}))
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

// EX1/EX2 (C1): a mixed turn whose trailing block is a REAL user message frames it as the user's
// instruction AND sets inp["userText"]; a trailing block that is purely an auto-injected
// <system-reminder> uses neutral framing and does NOT set userText (a reminder is context, not an
// instruction, so it must never drive a fresh send).
func TestComposerMixedTurnSystemReminderFraming(t *testing.T) {
	// Real trailing user message.
	real := composerInput([]byte(`{"messages":[
		{"role":"assistant","content":"","tool_calls":[{"id":"tc_1","function":{"name":"Read"}}]},
		{"role":"tool","tool_call_id":"tc_1","content":"R"},
		{"role":"user","content":"also do X"}
	]}`))
	if real["userText"] != "also do X" {
		t.Fatalf("real trailing user message must set userText, got %v", real["userText"])
	}
	results, _ := real["results"].([]map[string]any)
	if c, _ := results[len(results)-1]["content"].(string); !strings.Contains(c, "their latest instruction") || !strings.Contains(c, "also do X") {
		t.Fatalf("real trailing message must use instruction framing: %q", c)
	}

	// M30: an AUTO-INJECTED reminder (the SAME block recurs verbatim earlier in the transcript as context)
	// uses neutral framing and does NOT set userText. Here the reminder appears in an earlier user turn AND as
	// the trailing block — that recurrence is the synthetic signal.
	rem := composerInput([]byte(`{"messages":[
		{"role":"user","content":"<system-reminder>The file changed on disk.</system-reminder> please read it"},
		{"role":"assistant","content":"","tool_calls":[{"id":"tc_1","function":{"name":"Read"}}]},
		{"role":"tool","tool_call_id":"tc_1","content":"R"},
		{"role":"user","content":"<system-reminder>The file changed on disk.</system-reminder>"}
	]}`))
	if _, ok := rem["userText"]; ok {
		t.Fatalf("an auto-injected (recurring) system-reminder must NOT set userText, got %v", rem["userText"])
	}
	rres, _ := rem["results"].([]map[string]any)
	c, _ := rres[len(rres)-1]["content"].(string)
	if !strings.Contains(c, "[System reminder accompanying these tool results:]") {
		t.Fatalf("auto-injected system-reminder must use neutral framing, got %q", c)
	}
	if strings.Contains(c, "their latest instruction") {
		t.Fatalf("auto-injected system-reminder must NOT be labeled as the user's instruction, got %q", c)
	}

	// M30: a FIRST-occurrence (user-authored) <system-reminder> as the trailing message is NOT auto-injected
	// context — it is the user's literal message and MUST be answerable (userText set, instruction framing).
	lit := composerInput([]byte(`{"messages":[
		{"role":"assistant","content":"","tool_calls":[{"id":"tc_1","function":{"name":"Read"}}]},
		{"role":"tool","tool_call_id":"tc_1","content":"R"},
		{"role":"user","content":"<system-reminder>summarize what you just read</system-reminder>"}
	]}`))
	if lit["userText"] == nil || !strings.Contains(lit["userText"].(string), "summarize what you just read") {
		t.Fatalf("a literal first-occurrence user <system-reminder> must set userText (M30), got %v", lit["userText"])
	}
	lres, _ := lit["results"].([]map[string]any)
	lc, _ := lres[len(lres)-1]["content"].(string)
	if !strings.Contains(lc, "their latest instruction") {
		t.Fatalf("a literal user reminder must use instruction framing (M30), got %q", lc)
	}

	// isAutoInjectedReminder unit checks: a single occurrence is user text (false); a recurrence is synthetic.
	single := gjson.GetBytes([]byte(`{"messages":[{"role":"user","content":"<system-reminder>x</system-reminder>"}]}`), "messages").Array()
	if isAutoInjectedReminder("<system-reminder>x</system-reminder>", single) {
		t.Fatalf("a single-occurrence reminder must be treated as user text (M30)")
	}
	recurring := gjson.GetBytes([]byte(`{"messages":[{"role":"user","content":"<system-reminder>x</system-reminder> hi"},{"role":"user","content":"<system-reminder>x</system-reminder>"}]}`), "messages").Array()
	if !isAutoInjectedReminder("<system-reminder>x</system-reminder>", recurring) {
		t.Fatalf("a recurring reminder block must be classified auto-injected")
	}

	// isPureSystemReminder unit checks: a reminder plus a real sentence is NOT pure.
	if !isPureSystemReminder("  <system-reminder>x</system-reminder> ") {
		t.Fatalf("a lone reminder block must be classified pure")
	}
	if isPureSystemReminder("<system-reminder>x</system-reminder> now actually do this") {
		t.Fatalf("a reminder followed by real text must NOT be pure")
	}
	if isPureSystemReminder("just a normal message") {
		t.Fatalf("plain text must not be a system reminder")
	}
}

// EX3/C5: a role:tool message that carries an image part yields a result map with images; one marked
// is_error:true yields isError:true (camelCase on the bridge wire); a clean result carries neither.
func TestComposerToolResultImageAndIsError(t *testing.T) {
	cont := composerInput([]byte(`{"messages":[
		{"role":"assistant","content":"","tool_calls":[{"id":"tc_1","function":{"name":"Screenshot"}},{"id":"tc_2","function":{"name":"Read"}}]},
		{"role":"tool","tool_call_id":"tc_1","content":[{"type":"text","text":"shot"},{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD"}}]},
		{"role":"tool","tool_call_id":"tc_2","content":"plain","is_error":true}
	]}`))
	results, _ := cont["results"].([]map[string]any)
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	imgs, _ := results[0]["images"].([]map[string]any)
	if len(imgs) != 1 || imgs[0]["data"] != "QUJD" || imgs[0]["mimeType"] != "image/png" {
		t.Fatalf("tool-result image not extracted: %#v", results[0]["images"])
	}
	if _, ok := results[0]["isError"]; ok {
		t.Fatalf("a non-error result must NOT carry isError")
	}
	if results[1]["isError"] != true {
		t.Fatalf("is_error:true must surface as isError:true (camelCase), got %v", results[1]["isError"])
	}
	if _, ok := results[1]["images"]; ok {
		t.Fatalf("a text-only result must NOT carry images")
	}
}

// EX4/EX11/EX12 (C4): extractComposerImages emits the inline {data,mimeType} form for a data: URI, the URL
// {url[,mimeType]} form for an http(s) image, and SKIPS degenerate attachments (empty data / empty mime /
// empty url) so one bad attachment never fails the turn.
func TestExtractComposerImagesForms(t *testing.T) {
	m := gjson.Parse(`{"role":"user","content":[
		{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD"}},
		{"type":"image_url","image_url":{"url":"https://example.com/pic.jpg"}},
		{"type":"image_url","image_url":{"url":"https://example.com/noext"}},
		{"type":"image_url","image_url":{"url":"data:image/png;base64,"}},
		{"type":"image_url","image_url":{"url":"data:;base64,QUJD"}}
	]}`)
	imgs := extractComposerImages(m)
	if len(imgs) != 3 {
		t.Fatalf("expected 3 valid images (2 degenerate skipped), got %d: %#v", len(imgs), imgs)
	}
	if imgs[0]["data"] != "QUJD" || imgs[0]["mimeType"] != "image/png" {
		t.Fatalf("inline image wrong: %#v", imgs[0])
	}
	if imgs[1]["url"] != "https://example.com/pic.jpg" || imgs[1]["mimeType"] != "image/jpeg" {
		t.Fatalf("URL image with extension wrong: %#v", imgs[1])
	}
	if imgs[2]["url"] != "https://example.com/noext" {
		t.Fatalf("URL image without extension wrong: %#v", imgs[2])
	}
	if _, ok := imgs[2]["mimeType"]; ok {
		t.Fatalf("URL image without a known extension must omit mimeType: %#v", imgs[2])
	}
}

// EX4/EX14: a mixed continuation whose trailing user message carries an image (e.g. background-then-type)
// must carry that image on the continuation input so the bridge's fresh send can attach it.
func TestComposerContinuationTrailingImage(t *testing.T) {
	cont := composerInput([]byte(`{"messages":[
		{"role":"assistant","content":"","tool_calls":[{"id":"tc_1","function":{"name":"Read"}}]},
		{"role":"tool","tool_call_id":"tc_1","content":"R"},
		{"role":"user","content":[{"type":"text","text":"look at this"},{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD"}}]}
	]}`))
	if cont["type"] != "tool_results" {
		t.Fatalf("mixed turn must classify as tool_results, got %v", cont["type"])
	}
	imgs, _ := cont["images"].([]map[string]any)
	if len(imgs) != 1 || imgs[0]["data"] != "QUJD" {
		t.Fatalf("trailing continuation image not carried: %#v", cont["images"])
	}
	if cont["userText"] != "look at this" {
		t.Fatalf("trailing user text must still be set: %v", cont["userText"])
	}
}

// EX5: each additional conv-id signal (Session_id / Conversation_id / X-Amp-Thread-Id / X-Client-Request-Id
// headers and body conversation_id / prompt_cache_key / previous_response_id) yields a stable session id;
// the same conv id is stable across content changes.
func TestDeriveComposerSessionID_ExtendedConvSignals(t *testing.T) {
	auth := authWith("authA", "keyA")
	// ADD-48: X-Client-Request-Id is no longer in the stable set (it is a per-request tracing id). Only true
	// conversation/session/thread headers are stable.
	headerCases := []string{"Session_id", "Conversation_id", "X-Amp-Thread-Id"}
	for _, h := range headerCases {
		o1 := optsWithHeaders(map[string]string{h: "cid-" + h})
		s1, err := deriveComposerSessionID(auth, "cursorkey", toolTurn("hello"), o1)
		if err != nil || !strings.HasPrefix(s1, "sess_") {
			t.Fatalf("header %s should route (id=%q err=%v)", h, s1, err)
		}
		s2, _ := deriveComposerSessionID(auth, "cursorkey", toolTurn("different content"), o1)
		if s1 != s2 {
			t.Fatalf("header %s: same conv id must be stable across content (%s vs %s)", h, s1, s2)
		}
	}
	// ADD-48: a request-id-only turn (X-Client-Request-Id, metadata request_id) must NOT be treated as a stable
	// conv id — each is per-request, so stableConversationID returns "" and the turn mints a fresh session.
	if got := stableConversationID(optsWithHeaders(map[string]string{"X-Client-Request-Id": "req-123"})); got != "" {
		t.Fatalf("ADD-48: X-Client-Request-Id must not be a stable conv id, got %q", got)
	}
	if got := stableConversationID(cliproxyexecutor.Options{Metadata: map[string]any{"request_id": "req-abc"}}); got != "" {
		t.Fatalf("ADD-48: metadata request_id must not be a stable conv id, got %q", got)
	}
	if got := stableConversationID(cliproxyexecutor.Options{Metadata: map[string]any{"requestId": "req-xyz"}}); got != "" {
		t.Fatalf("ADD-48: metadata requestId must not be a stable conv id, got %q", got)
	}
	// H16: previous_response_id is INTENTIONALLY NOT in this stable set — it changes every turn, so it is no
	// longer hashed as a stable conv id; it routes via the response-id->sessionID map instead (covered by
	// TestDeriveComposerSessionID_PreviousResponseIDResumes). ADD-78: prompt_cache_key is ALSO no longer in the
	// stable set (it is a cache-locality hint, not a conversation identity). Only conversation_id stays stable.
	bodyCases := []string{"conversation_id"}
	for _, k := range bodyCases {
		payload := []byte(`{"` + k + `":"body-` + k + `"}`)
		o := cliproxyexecutor.Options{OriginalRequest: payload}
		s1, err := deriveComposerSessionID(auth, "cursorkey", toolTurn("hi"), o)
		if err != nil || !strings.HasPrefix(s1, "sess_") {
			t.Fatalf("body %s should route (id=%q err=%v)", k, s1, err)
		}
		// Same body id, different inbound message content => stable session.
		s2, _ := deriveComposerSessionID(auth, "cursorkey", toolTurn("totally other"), o)
		if s1 != s2 {
			t.Fatalf("body %s: same conv id must be stable across content (%s vs %s)", k, s1, s2)
		}
	}
	// H16: a bare previous_response_id with NO recorded mapping must NOT be a stable conv-id hash — it mints a
	// fresh session (graceful fallback). Two such calls therefore differ (proving it left stableConversationID).
	prevOpts := cliproxyexecutor.Options{OriginalRequest: []byte(`{"previous_response_id":"unmapped-prev"}`)}
	p1, _ := deriveComposerSessionID(auth, "cursorkey", toolTurn("hi"), prevOpts)
	p2, _ := deriveComposerSessionID(auth, "cursorkey", toolTurn("hi"), prevOpts)
	if p1 == p2 {
		t.Fatalf("H16: an unmapped previous_response_id must NOT be hashed as a stable conv id (must mint fresh): %s == %s", p1, p2)
	}
	// Distinct conv ids (here via Session_id) must differ.
	a, _ := deriveComposerSessionID(auth, "cursorkey", toolTurn("x"), optsWithHeaders(map[string]string{"Session_id": "A"}))
	b, _ := deriveComposerSessionID(auth, "cursorkey", toolTurn("x"), optsWithHeaders(map[string]string{"Session_id": "B"}))
	if a == b {
		t.Fatalf("distinct Session_id values must differ: %s == %s", a, b)
	}
	// stableConversationID must never derive from message text: no signals => empty.
	if got := stableConversationID(cliproxyexecutor.Options{OriginalRequest: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)}); got != "" {
		t.Fatalf("stableConversationID must be empty when no id signal is present, got %q", got)
	}
}

// EX6: a continuation with no recorded emitter AND no stable id mints a fresh session, never errors.
func TestDeriveComposerSessionID_ContinuationMintsOnMiss(t *testing.T) {
	auth := authWith("authA", "keyA")
	cont := []byte(`{"messages":[
		{"role":"assistant","tool_calls":[{"id":"tc_orphan"}]},
		{"role":"tool","tool_call_id":"tc_orphan","content":"R"}
	]}`)
	got, err := deriveComposerSessionID(auth, "cursorkey", cont, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("orphan continuation must mint, not error (EX6): %v", err)
	}
	if !strings.HasPrefix(got, "sess_") {
		t.Fatalf("orphan continuation must mint a fresh sess_ id, got %q", got)
	}
}

// EX7 (C2) / H13: composerHistoryFingerprint hashes the first two non-system messages — GROWTH-STABLE
// (appending turns at the tail does NOT change it for a >=2-message conversation) but REWRITE-SENSITIVE (a
// /compact that rewrites the body surfacing as message index 1 changes it). Ignores system/developer
// messages and is emitted on BOTH the user and tool_results inputs.
func TestComposerHistoryFingerprint(t *testing.T) {
	base := gjson.GetBytes([]byte(`{"messages":[{"role":"system","content":"S"},{"role":"user","content":"a"},{"role":"assistant","content":"b"}]}`), "messages").Array()
	fp1 := composerHistoryFingerprint(base)
	if len(fp1) != 32 {
		t.Fatalf("fingerprint must be 32 hex chars, got %q", fp1)
	}
	same := gjson.GetBytes([]byte(`{"messages":[{"role":"system","content":"DIFFERENT SYSTEM"},{"role":"user","content":"a"},{"role":"assistant","content":"b"}]}`), "messages").Array()
	if composerHistoryFingerprint(same) != fp1 {
		t.Fatalf("changing only a system message must NOT change the fingerprint")
	}
	changed := gjson.GetBytes([]byte(`{"messages":[{"role":"system","content":"S"},{"role":"user","content":"a CHANGED"},{"role":"assistant","content":"b"}]}`), "messages").Array()
	if composerHistoryFingerprint(changed) == fp1 {
		t.Fatalf("changing a non-system message MUST change the fingerprint")
	}
	if composerHistoryFingerprint(gjson.GetBytes([]byte(`{"messages":[{"role":"system","content":"only system"}]}`), "messages").Array()) != "" {
		t.Fatalf("no non-system content => empty fingerprint")
	}
	// GROWTH-STABLE (P1 regression guard): APPENDING new turns while the opener is unchanged must NOT change
	// the fingerprint — otherwise a stateful client (Claude Code) would re-seed the whole history every turn
	// and race ERROR_CONVERSATION_TOO_LONG. Only a /compact-style head rewrite (the `changed` case above) does.
	appended := gjson.GetBytes([]byte(`{"messages":[{"role":"system","content":"S"},{"role":"user","content":"a"},{"role":"assistant","content":"b"},{"role":"user","content":"c"},{"role":"assistant","content":"d"}]}`), "messages").Array()
	if composerHistoryFingerprint(appended) != fp1 {
		t.Fatalf("appending new turns must NOT change the fingerprint (growth-stable)")
	}
	// Emitted on the new-user input.
	user := composerInput([]byte(`{"messages":[{"role":"user","content":"hi"}]}`))
	if fp, _ := user["historyFingerprint"].(string); len(fp) != 32 {
		t.Fatalf("user input must carry a 32-hex historyFingerprint, got %q", fp)
	}
	// Emitted on the tool_results continuation input.
	cont := composerInput([]byte(`{"messages":[{"role":"assistant","tool_calls":[{"id":"tc_1"}]},{"role":"tool","tool_call_id":"tc_1","content":"R"}]}`))
	if fp, _ := cont["historyFingerprint"].(string); len(fp) != 32 {
		t.Fatalf("tool_results input must carry a 32-hex historyFingerprint, got %q", fp)
	}
}

// EX8 (C3) + EX10: a tool_results continuation carries the extracted system and a bounded rendered history
// so the bridge can re-seed a fresh/evicted session before applying the results.
func TestComposerContinuationCarriesSystemAndHistory(t *testing.T) {
	cont := composerInput([]byte(`{"messages":[
		{"role":"system","content":"updated system after ExitPlanMode"},
		{"role":"user","content":"read the file"},
		{"role":"assistant","content":"","tool_calls":[{"id":"tc_1","function":{"name":"Read","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"tc_1","content":"package main"}
	]}`))
	if cont["type"] != "tool_results" {
		t.Fatalf("expected tool_results, got %v", cont["type"])
	}
	if cont["system"] != "updated system after ExitPlanMode" {
		t.Fatalf("continuation must carry the swapped system (C3), got %v", cont["system"])
	}
	hist, _ := cont["history"].(string)
	if !strings.Contains(hist, "USER: read the file") {
		t.Fatalf("continuation must carry rendered pre-tool-call history (EX10), got %q", hist)
	}
	// The trailing tool result the continuation is answering should NOT be re-rendered into the seeded history
	// (continuationHistoryBound stops before the last assistant tool_calls turn).
	if strings.Contains(hist, "package main") {
		t.Fatalf("the answered tool result must not be duplicated into seeded history, got %q", hist)
	}
}

// ADD-67 (reverses EX9): assistant reasoning_content is NOT replayed verbatim by default — it becomes a
// neutral omission marker. The assistant answer text still appears. (Verbatim replay is restorable behind
// CURSOR_COMPOSER_REPLAY_REASONING=1; covered by TestADD67_ReasoningNotReplayedByDefault.)
func TestRenderComposerHistoryOmitsReasoningByDefault(t *testing.T) {
	messages := gjson.GetBytes([]byte(`{"messages":[
		{"role":"user","content":"q"},
		{"role":"assistant","content":"answer","reasoning_content":"because of X and Y"},
		{"role":"user","content":"next"}
	]}`), "messages").Array()
	h := renderComposerHistory(messages, 2)
	if strings.Contains(h, "because of X and Y") {
		t.Fatalf("ADD-67: raw reasoning must NOT appear in rendered history: %q", h)
	}
	if !strings.Contains(h, "[assistant reasoning omitted]") {
		t.Fatalf("ADD-67: an omission marker must replace replayed reasoning: %q", h)
	}
	if !strings.Contains(h, "ASSISTANT: answer") {
		t.Fatalf("assistant text must still appear: %q", h)
	}
	// A bare reasoning-only assistant turn (no text, no tool calls) must still render the omission marker (so
	// positional context survives) — not be silently dropped.
	only := gjson.GetBytes([]byte(`{"messages":[{"role":"user","content":"q"},{"role":"assistant","content":"","reasoning_content":"silent thought"},{"role":"user","content":"n"}]}`), "messages").Array()
	h2 := renderComposerHistory(only, 2)
	if strings.Contains(h2, "silent thought") {
		t.Fatalf("ADD-67: bare reasoning-only turn must not leak raw reasoning: %q", h2)
	}
	if !strings.Contains(h2, "[assistant reasoning omitted]") {
		t.Fatalf("ADD-67: reasoning-only assistant turn must still render the omission marker: %q", h2)
	}
}

// EX13: an oversized historical tool output is truncated in the rendered history so a re-seed cannot blow
// Cursor's per-message limit (ERROR_CONVERSATION_TOO_LONG).
func TestRenderComposerHistoryTruncatesToolOutput(t *testing.T) {
	huge := strings.Repeat("A", 50_000)
	payload := `{"messages":[{"role":"user","content":"q"},{"role":"assistant","tool_calls":[{"id":"tc_1","function":{"name":"Read"}}]},{"role":"tool","tool_call_id":"tc_1","content":"` + huge + `"},{"role":"user","content":"next"}]}`
	messages := gjson.GetBytes([]byte(payload), "messages").Array()
	h := renderComposerHistory(messages, 3)
	if len(h) >= len(huge) {
		t.Fatalf("oversized tool output must be truncated in history (rendered len=%d, raw=%d)", len(h), len(huge))
	}
	if !strings.Contains(h, "TOOL[tc_1]:") {
		t.Fatalf("tool result still tagged: %q", h[:min(200, len(h))])
	}
}

// EX15: an image-only turn (no text) emits a placeholder line in rendered history instead of being dropped,
// keeping positional context for the model on a re-seed.
func TestRenderComposerHistoryImagePlaceholder(t *testing.T) {
	messages := gjson.GetBytes([]byte(`{"messages":[
		{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD"}}]},
		{"role":"assistant","content":"I see it"},
		{"role":"user","content":"thanks"}
	]}`), "messages").Array()
	h := renderComposerHistory(messages, 2)
	if !strings.Contains(h, "USER: [image x1: image/png]") {
		t.Fatalf("image-only turn must emit a placeholder in history: %q", h)
	}
}

// ---- all-35 principal-eng audit regression tests (executor-owned findings) ----

// H10 (C-CONTINUATION-TOOLS): composerTurnBody must attach `tools` on EVERY turn when advertised, not only
// on a new-user turn — the bridge refreshes its advertise set from body.tools on tool_results turns too, so
// dropping tools on a continuation left a stale advertise set. tool_choice=none (advertise=nil) still omits.
func TestComposerTurnBodyToolsOnContinuation(t *testing.T) {
	adv := []map[string]any{{"name": "Read", "toolName": "Read"}}
	// A tool_results continuation MUST still carry the current tool inventory.
	cont := composerTurnBody("s1", "composer-2.5", map[string]any{"type": "tool_results", "results": []any{}}, adv, "auto", nil, nil)
	if gjson.GetBytes(cont, "tools.0.name").String() != "Read" {
		t.Fatalf("H10: continuation turn must include the current tools array, got %s", cont)
	}
	// A new-user turn still carries tools (unchanged behavior).
	user := composerTurnBody("s1", "composer-2.5", map[string]any{"type": "user", "text": "x"}, adv, "auto", nil, nil)
	if gjson.GetBytes(user, "tools.0.name").String() != "Read" {
		t.Fatalf("H10: user turn must include tools, got %s", user)
	}
	// tool_choice=none gating (advertise=nil) still wins — no tools on either turn type.
	none := composerTurnBody("s1", "composer-2.5", map[string]any{"type": "tool_results", "results": []any{}}, nil, "none", nil, nil)
	if gjson.GetBytes(none, "tools").Exists() {
		t.Fatalf("H10: tool_choice=none must still omit tools even on a continuation, got %s", none)
	}
}

// M29: multi-block text content must be joined with a newline, not bare-concatenated (which would corrupt
// code/command-output/JSON boundaries: "foo"+"bar" -> "foobar").
func TestCursorMessageTextJoinsBlocks(t *testing.T) {
	m := gjson.Parse(`{"role":"user","content":[{"type":"text","text":"foo"},{"type":"text","text":"bar"}]}`)
	if got := cursorMessageText(m); got != "foo\nbar" {
		t.Fatalf("M29: multi-block text must join with newline, got %q", got)
	}
	// A single block is unchanged; a string content is unchanged.
	if got := cursorMessageText(gjson.Parse(`{"role":"user","content":[{"type":"text","text":"only"}]}`)); got != "only" {
		t.Fatalf("M29: single block must not gain a separator, got %q", got)
	}
	if got := cursorMessageText(gjson.Parse(`{"role":"user","content":"plain"}`)); got != "plain" {
		t.Fatalf("M29: string content must be unchanged, got %q", got)
	}
}

// M31: assistant tool-call ARGUMENTS replayed into seed history must be bounded by the same truncation as
// tool RESULTS, so a huge prior-call argument cannot blow Cursor's per-message limit on a re-seed.
func TestRenderComposerHistoryTruncatesToolCallArgs(t *testing.T) {
	huge := strings.Repeat("A", 50_000)
	payload := `{"messages":[{"role":"user","content":"q"},{"role":"assistant","tool_calls":[{"id":"tc_1","function":{"name":"Write","arguments":"` + huge + `"}}]},{"role":"user","content":"next"}]}`
	messages := gjson.GetBytes([]byte(payload), "messages").Array()
	h := renderComposerHistory(messages, 2)
	if len(h) >= len(huge) {
		t.Fatalf("M31: oversized tool-call args must be truncated in history (rendered len=%d, raw=%d)", len(h), len(huge))
	}
	if !strings.Contains(h, "tc_1:Write(") {
		t.Fatalf("M31: tool-call intent must still render (just bounded), got %q", h[:min(200, len(h))])
	}
}

// H13: composerHistoryFingerprint must be GROWTH-STABLE (tail appends do not change it) but REWRITE-SENSITIVE
// — a /compact that REWRITES the body while PRESERVING the first user message verbatim MUST change it (the
// old "first non-system message only" anchor missed exactly this).
func TestComposerHistoryFingerprintRewriteSensitive(t *testing.T) {
	original := gjson.GetBytes([]byte(`{"messages":[
		{"role":"user","content":"open the project"},
		{"role":"assistant","content":"sure, here is a long exploration of the codebase ..."},
		{"role":"user","content":"now fix the bug in foo.go"},
		{"role":"assistant","content":"fixed it by changing the parser"}
	]}`), "messages").Array()
	fpOrig := composerHistoryFingerprint(original)

	// Tail append: same head, two more turns. Must NOT change (growth-stable).
	appended := gjson.GetBytes([]byte(`{"messages":[
		{"role":"user","content":"open the project"},
		{"role":"assistant","content":"sure, here is a long exploration of the codebase ..."},
		{"role":"user","content":"now fix the bug in foo.go"},
		{"role":"assistant","content":"fixed it by changing the parser"},
		{"role":"user","content":"thanks, also add a test"},
		{"role":"assistant","content":"added"}
	]}`), "messages").Array()
	if composerHistoryFingerprint(appended) != fpOrig {
		t.Fatalf("H13: tail append must NOT change the fingerprint (growth-stable)")
	}

	// /compact that PRESERVES the first user message verbatim but REWRITES the body into a summary. The old
	// first-message anchor would NOT change; the new bounded-prefix fingerprint MUST.
	compacted := gjson.GetBytes([]byte(`{"messages":[
		{"role":"user","content":"open the project"},
		{"role":"assistant","content":"[summary] We explored the codebase and fixed a parser bug in foo.go."},
		{"role":"user","content":"now fix the bug in foo.go"},
		{"role":"assistant","content":"fixed it by changing the parser"}
	]}`), "messages").Array()
	if composerHistoryFingerprint(compacted) == fpOrig {
		t.Fatalf("H13: a /compact that rewrites the body (preserving the opener) MUST change the fingerprint")
	}
}

// H16 (C-RESPID): a Responses/Codex text-only follow-up carrying previous_response_id must resume the
// DURABLE session recorded against that response id — NOT be diverted to an ephemeral one-shot, NOT be
// hashed as a stable id. On a map MISS it degrades gracefully (mints/hashes), never errors.
func TestDeriveComposerSessionID_PreviousResponseIDResumes(t *testing.T) {
	auth := authWith("authA", "keyA")
	tenant := composerTenant(auth, cliproxyexecutor.Options{})
	// Simulate a prior turn having recorded its outward response id -> session.
	prior := "sess_durable000000000000000000000000"
	recordComposerResponseSession(tenant, "chatcmpl-composer-resp1", prior)
	// A text-only follow-up (no tools) carrying previous_response_id must resume `prior`.
	followup := cliproxyexecutor.Options{OriginalRequest: []byte(`{"previous_response_id":"chatcmpl-composer-resp1","input":"thanks"}`)}
	got, err := deriveComposerSessionID(auth, "cursorkey", []byte(`{"messages":[{"role":"user","content":"thanks"}]}`), followup)
	if err != nil {
		t.Fatalf("H16: follow-up must not error: %v", err)
	}
	if got != prior {
		t.Fatalf("H16: previous_response_id must resume the durable session %s, got %s", prior, got)
	}
	// Unknown previous_response_id (bridge restart / eviction) must degrade gracefully (mint), not error.
	miss := cliproxyexecutor.Options{OriginalRequest: []byte(`{"previous_response_id":"chatcmpl-composer-UNKNOWN","input":"hi"}`)}
	gotMiss, errMiss := deriveComposerSessionID(auth, "cursorkey", []byte(`{"messages":[{"role":"user","content":"hi"}]}`), miss)
	if errMiss != nil || !strings.HasPrefix(gotMiss, "sess_") {
		t.Fatalf("H16: unknown previous_response_id must mint gracefully (id=%q err=%v)", gotMiss, errMiss)
	}
}

// H16/L35: a tool-less follow-up that carries an EXPLICIT body durable reference (previous_response_id or
// conversation_id) must NOT be classified as a utility one-shot. A bare history-only tool-less turn under a
// HEADER conv id STAYS a one-shot (concurrent-collision guard — see isComposerUtilityOneShot doc).
func TestIsComposerUtilityOneShotExclusions(t *testing.T) {
	// Pure tool-less probe with a user-only transcript IS a one-shot.
	if !isComposerUtilityOneShot([]byte(`{"messages":[{"role":"user","content":"title this"}]}`), cliproxyexecutor.Options{}, composerContinuationHint{}) {
		t.Fatalf("a tool-less user-only probe must classify as a utility one-shot")
	}
	// previous_response_id present -> NOT a one-shot (H16).
	if isComposerUtilityOneShot([]byte(`{"messages":[{"role":"user","content":"thanks"}]}`),
		cliproxyexecutor.Options{OriginalRequest: []byte(`{"previous_response_id":"r1"}`)}, composerContinuationHint{}) {
		t.Fatalf("H16: a follow-up with previous_response_id must NOT be a one-shot")
	}
	// conversation_id present -> NOT a one-shot (H16/L35).
	if isComposerUtilityOneShot([]byte(`{"messages":[{"role":"user","content":"thanks"}]}`),
		cliproxyexecutor.Options{OriginalRequest: []byte(`{"conversation_id":"c1"}`)}, composerContinuationHint{}) {
		t.Fatalf("H16/L35: a follow-up with conversation_id must NOT be a one-shot")
	}
	// Tool-less with assistant history but NO body durable reference STAYS a one-shot (concurrent throwaway
	// summary shape — routing it to the stable session would reintroduce the 409 collision the isolation guards).
	if !isComposerUtilityOneShot([]byte(`{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"hello"},{"role":"user","content":"summarize"}]}`), cliproxyexecutor.Options{}, composerContinuationHint{}) {
		t.Fatalf("a history-only tool-less turn with no body durable reference must remain a one-shot (concurrency guard)")
	}
}

// L35/H16 end-to-end: a tool-less follow-up carrying a BODY conversation_id routes to the durable session
// for that conversation, not an isolated ephemeral one.
func TestDeriveComposerSessionID_ToollessBodyConvFollowup(t *testing.T) {
	auth := authWith("authA", "keyA")
	agentic := []byte(`{"conversation_id":"conv-l35-body","tools":[{"type":"function","function":{"name":"Read"}}],"messages":[{"role":"user","content":"describe the repo"}]}`)
	bodyOpts := cliproxyexecutor.Options{OriginalRequest: []byte(`{"conversation_id":"conv-l35-body"}`)}
	stable, _ := deriveComposerSessionID(auth, "cursorkey", agentic, bodyOpts)
	toolless := []byte(`{"conversation_id":"conv-l35-body","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"hello"},{"role":"user","content":"summarize"}]}`)
	got, err := deriveComposerSessionID(auth, "cursorkey", toolless, bodyOpts)
	if err != nil {
		t.Fatalf("L35 follow-up must not error: %v", err)
	}
	if got != stable {
		t.Fatalf("L35/H16: a tool-less follow-up with a body conversation_id must route to the stable session %s, got %s", stable, got)
	}
}

// M32: a continuation batch whose tool_call_ids were emitted by DIFFERENT sessions must surface an explicit
// mixed-session error — never silently route the whole batch to one session (stranding the other's result).
func TestDeriveComposerSessionID_MixedSessionBatchErrors(t *testing.T) {
	auth := authWith("authB", "keyB")
	tenant := composerTenant(auth, cliproxyexecutor.Options{})
	recordComposerToolCall(tenant, "tc_sessA", "sess_A0000000000000000000000000000000")
	recordComposerToolCall(tenant, "tc_sessB", "sess_B0000000000000000000000000000000")
	mixed := []byte(`{"messages":[
		{"role":"assistant","tool_calls":[{"id":"tc_sessA"},{"id":"tc_sessB"}]},
		{"role":"tool","tool_call_id":"tc_sessA","content":"RA"},
		{"role":"tool","tool_call_id":"tc_sessB","content":"RB"}
	]}`)
	_, err := deriveComposerSessionID(auth, "cursorkey", mixed, cliproxyexecutor.Options{})
	if err == nil {
		t.Fatalf("M32: a mixed-session batch must error, not route partially")
	}
	if !strings.Contains(err.Error(), "multiple sessions") {
		t.Fatalf("M32: error must identify the mixed-session cause, got %v", err)
	}
	// A batch whose recognized ids all belong to the SAME session routes normally (no error).
	recordComposerToolCall(tenant, "tc_same1", "sess_S0000000000000000000000000000000")
	recordComposerToolCall(tenant, "tc_same2", "sess_S0000000000000000000000000000000")
	same := []byte(`{"messages":[
		{"role":"assistant","tool_calls":[{"id":"tc_same1"},{"id":"tc_same2"}]},
		{"role":"tool","tool_call_id":"tc_same1","content":"R1"},
		{"role":"tool","tool_call_id":"tc_same2","content":"R2"}
	]}`)
	got, errSame := deriveComposerSessionID(auth, "cursorkey", same, cliproxyexecutor.Options{})
	if errSame != nil {
		t.Fatalf("M32: a same-session batch must route, not error: %v", errSame)
	}
	if got != "sess_S0000000000000000000000000000000" {
		t.Fatalf("M32: same-session batch must route to that session, got %s", got)
	}
}

// H07/H22: an allowed_tools allow-list restricts the advertised set to the intersection; an EMPTY
// intersection surfaces explicit-unsupported (advertise nothing + unsupported note) — never widen to all.
func TestApplyComposerAllowedTools(t *testing.T) {
	adv := []map[string]any{{"name": "Read"}, {"name": "Bash"}, {"name": "Write"}}
	defs := []cursorToolDefinition{{Name: "Read"}, {Name: "Bash"}, {Name: "Write"}}
	// Allow only Read + Bash.
	oai := []byte(`{"allowed_tools":{"type":"allowed_tools","mode":"auto","tools":[{"type":"function","name":"Read"},{"type":"function","name":"Bash"}]}}`)
	got, unsupported := applyComposerAllowedTools(adv, oai, defs, nil)
	if unsupported {
		t.Fatalf("H07: a satisfiable allow-list must not be unsupported")
	}
	names := map[string]bool{}
	for _, a := range got {
		names[a["name"].(string)] = true
	}
	if len(got) != 2 || !names["Read"] || !names["Bash"] || names["Write"] {
		t.Fatalf("H07: allow-list must restrict to {Read,Bash}, got %#v", got)
	}
	// No allow_tools => unchanged, not unsupported.
	g2, u2 := applyComposerAllowedTools(adv, []byte(`{}`), defs, nil)
	if u2 || len(g2) != 3 {
		t.Fatalf("H07: absent allow-list must pass through all tools, got %d unsupported=%v", len(g2), u2)
	}
	// Allow-list referencing an unadvertised tool => EMPTY intersection => unsupported, advertise nothing.
	empty := []byte(`{"allowed_tools":{"type":"allowed_tools","tools":[{"type":"function","name":"NonExistent"}]}}`)
	g3, u3 := applyComposerAllowedTools(adv, empty, defs, nil)
	if !u3 || len(g3) != 0 {
		t.Fatalf("H07: empty intersection must be unsupported with no tools (never widen to all), got %d unsupported=%v", len(g3), u3)
	}
}

// H20/H21: hard guarantees the composer path cannot enforce server-side are flagged in
// unsupportedHardGuarantees — never silently pretended-enforced. json_schema/stop/max_tokens additionally
// carry a bridge-CONSUMED field; parallel_tool_calls:false does NOT (the bridge has no parallelToolCalls knob),
// so it is advisory-only and must NOT be written as a dead body field.
func TestComposerConstraintsUnsupportedSignals(t *testing.T) {
	c := composerConstraints([]byte(`{"response_format":{"type":"json_schema","json_schema":{"name":"x"}},"stop":["END"],"max_tokens":128,"parallel_tool_calls":false}`))
	notes, _ := c["unsupportedHardGuarantees"].([]string)
	if len(notes) == 0 {
		t.Fatalf("H20/H21: hard-guarantee requests must produce unsupported notes, got none: %#v", c)
	}
	joined := strings.Join(notes, " | ")
	for _, want := range []string{"json_schema", "stop", "max_tokens", "parallel_tool_calls=false"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("H20/H21: missing unsupported note for %q in %q", want, joined)
		}
	}
	// Dead-key regression guard: the bridge's constraints allowlist never reads parallelToolCalls, so it must
	// NOT be written back as a dedicated (dead) body field — the signal lives only in unsupportedHardGuarantees.
	if _, ok := c["parallelToolCalls"]; ok {
		t.Fatalf("H20: parallel_tool_calls=false must NOT be carried as a dead body field, got %v", c["parallelToolCalls"])
	}
	// A plain request with no hard guarantees produces no unsupported list.
	plain := composerConstraints([]byte(`{"response_format":{"type":"json_object"}}`))
	if _, ok := plain["unsupportedHardGuarantees"]; ok {
		t.Fatalf("H20/H21: a non-hard request (json_object only) must not flag unsupported, got %#v", plain)
	}
	// parallel_tool_calls:true needs no signal.
	if _, ok := composerConstraints([]byte(`{"parallel_tool_calls":true}`))["unsupportedHardGuarantees"]; ok {
		t.Fatalf("H20: parallel_tool_calls:true must not flag unsupported")
	}
}

// H22 (executor side): a Responses built-in tool (no `function` block, e.g. web_search) must NOT be silently
// dropped on the composer path — it has no Cursor equivalent, so it is surfaced as an unsupported guarantee.
// A normal function tool must NOT be flagged.
func TestComposerConstraintsBuiltinToolUnsupported(t *testing.T) {
	c := composerConstraints([]byte(`{"tools":[{"type":"web_search"},{"type":"function","function":{"name":"Read","parameters":{"type":"object"}}}]}`))
	notes, _ := c["unsupportedHardGuarantees"].([]string)
	joined := strings.Join(notes, " | ")
	if !strings.Contains(joined, "web_search") {
		t.Fatalf("H22: a built-in tool must surface an unsupported note, got %q", joined)
	}
	if strings.Contains(joined, "Read") {
		t.Fatalf("H22: a normal function tool must NOT be flagged unsupported, got %q", joined)
	}
	// Function-only tools (or none) produce no built-in note.
	if _, ok := composerConstraints([]byte(`{"tools":[{"type":"function","function":{"name":"Bash"}}]}`))["unsupportedHardGuarantees"]; ok {
		t.Fatalf("H22: a function-only tool set must not flag unsupported")
	}
}

// M25: bridge URL + error-body redaction. Secret-ish substrings (crsr_/sk-/bearer/signed-url/token) must be
// scrubbed; a URL's userinfo + secret query params must be redacted; a client-visible error must carry a
// correlation id and NOT the raw body.
func TestComposerRedaction(t *testing.T) {
	body := []byte(`{"error":"bad key crsr_abc123secret and sk-deadbeefcafef00d, Authorization: Bearer tok_xyz, url https://x/y?signature=SIGSECRET&foo=bar"}`)
	s := sanitizeBridgeBody(body)
	for _, leak := range []string{"crsr_abc123secret", "sk-deadbeefcafef00d", "tok_xyz", "SIGSECRET"} {
		if strings.Contains(s, leak) {
			t.Fatalf("M25: sanitizeBridgeBody leaked %q: %s", leak, s)
		}
	}
	if !strings.Contains(s, "[redacted]") {
		t.Fatalf("M25: sanitizeBridgeBody must mark redactions: %s", s)
	}
	// URL redaction: userinfo + secret query params.
	u := redactBridgeURL("https://user:p4ss@bridge.example.com/agent/turn?key=SECRETKEY&auth_token=TOK&keep=1")
	for _, leak := range []string{"p4ss", "SECRETKEY", "TOK"} {
		if strings.Contains(u, leak) {
			t.Fatalf("M25: redactBridgeURL leaked %q: %s", leak, u)
		}
	}
	if !strings.Contains(u, "bridge.example.com") || !strings.Contains(u, "keep=1") {
		t.Fatalf("M25: redactBridgeURL must keep host + non-secret params: %s", u)
	}
	// Correlation ids are non-empty and unique.
	if a, b := composerCorrelationID(), composerCorrelationID(); a == "" || a == b {
		t.Fatalf("M25: correlation ids must be non-empty and unique (a=%q b=%q)", a, b)
	}
}

// M25 end-to-end: a bridge NON-2xx whose body carries a secret must NOT leak the secret into the
// client-visible error; the error must carry only a status + correlation id.
func TestExecuteComposerStreamRedactsBridgeErrorBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"error":"upstream rejected key crsr_LEAKEDSECRET123"}`))
	}))
	defer srv.Close()
	e := NewCursorExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{ID: "t", Attributes: map[string]string{"api_key": "k", "composer_client_tools_bridge_url": srv.URL}}
	req := cliproxyexecutor.Request{Model: "composer-2.5", Payload: []byte(`{"model":"composer-2.5","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"Read"}}]}`)}
	_, err := e.executeComposerStream(context.Background(), auth, "k", req,
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai"), Headers: http.Header{"X-Conversation-Id": []string{"redact1"}}})
	if err == nil {
		t.Fatalf("M25: a bridge 500 must surface an error")
	}
	if strings.Contains(err.Error(), "crsr_LEAKEDSECRET123") {
		t.Fatalf("M25: the client-visible error must NOT contain the secret body: %v", err)
	}
	if !strings.Contains(err.Error(), "correlation") {
		t.Fatalf("M25: the client-visible error must carry a correlation id: %v", err)
	}
}

// M24 (C-KEEPALIVE): for OpenAI-Responses and Gemini inbound, the bridge's {"type":"ping"} must render to a
// REAL keepalive frame (a raw `: keepalive` SSE comment emitted directly by the executor) — not zero bytes —
// so a long/queued turn does not trip a client idle-abort. The comment must appear AFTER the stream envelope
// is open (after real content), never before it.
func TestExecuteComposerStreamKeepaliveResponsesAndGemini(t *testing.T) {
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
		write(`{"type":"text","delta":"hi"}`) // open the envelope first
		write(`{"type":"ping"}`)              // keepalive AFTER content
		write(`{"type":"turn_end","stop_reason":"stop"}`)
		write("[DONE]")
	}))
	defer srv.Close()
	e := NewCursorExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{ID: "authK", Attributes: map[string]string{"api_key": "k", "composer_client_tools_bridge_url": srv.URL}}
	collect := func(sr *cliproxyexecutor.StreamResult) string {
		var b strings.Builder
		for chunk := range sr.Chunks {
			if chunk.Err == nil {
				b.Write(chunk.Payload)
			}
		}
		return b.String()
	}
	for _, fmtName := range []string{"openai-response", "gemini"} {
		req := cliproxyexecutor.Request{Model: "composer-2.5", Payload: []byte(`{"model":"composer-2.5","messages":[{"role":"user","content":"hi"}]}`)}
		sr, err := e.executeComposerStream(context.Background(), auth, "k", req,
			cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString(fmtName), Headers: http.Header{"X-Conversation-Id": []string{"ka-" + fmtName}}})
		if err != nil {
			t.Fatalf("%s: stream setup: %v", fmtName, err)
		}
		out := collect(sr)
		if !strings.Contains(out, ": keepalive") {
			t.Fatalf("%s: M24 keepalive comment not emitted (would be zero bytes -> idle abort): %q", fmtName, out)
		}
	}
}

// M24: a ping that arrives BEFORE any content (queued-wait) must NOT emit a bare comment for
// Responses/Gemini (it would precede response.created / the first candidate). It falls through to the
// empty-delta path; the keepalive only fires once the envelope is open.
func TestExecuteComposerStreamKeepaliveNotBeforeEnvelope(t *testing.T) {
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
		write(`{"type":"ping"}`) // BEFORE any content
		write(`{"type":"text","delta":"hi"}`)
		write(`{"type":"turn_end","stop_reason":"stop"}`)
		write("[DONE]")
	}))
	defer srv.Close()
	e := NewCursorExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{ID: "authK2", Attributes: map[string]string{"api_key": "k", "composer_client_tools_bridge_url": srv.URL}}
	req := cliproxyexecutor.Request{Model: "composer-2.5", Payload: []byte(`{"model":"composer-2.5","messages":[{"role":"user","content":"hi"}]}`)}
	sr, err := e.executeComposerStream(context.Background(), auth, "k", req,
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response"), Headers: http.Header{"X-Conversation-Id": []string{"ka-pre"}}})
	if err != nil {
		t.Fatalf("stream setup: %v", err)
	}
	var first []byte
	for chunk := range sr.Chunks {
		if chunk.Err == nil && len(chunk.Payload) > 0 && first == nil {
			first = append([]byte(nil), chunk.Payload...)
		}
	}
	if strings.HasPrefix(string(first), ": keepalive") {
		t.Fatalf("M24: a pre-envelope ping must NOT emit the keepalive comment first, got %q", string(first))
	}
}

// ===========================================================================
// Combined addendum (ADD-36..ADD-75) regression tests — executor side.
// ===========================================================================

// ADD-59: a bridge non-2xx (here 410 for a lost tool-results continuation) must surface to the client as a
// typed StatusError carrying the bridge status, NOT a generic 500 — and never as a clean success. The 410
// message must signal a lost/re-seedable continuation (distinct from "retry later").
func TestADD59_BridgeStatusErrorPreservesCode(t *testing.T) {
	for _, mode := range []string{"stream", "nonstream"} {
		t.Run(mode, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(410)
				_, _ = w.Write([]byte("unknown or expired session: the tool call this result answers was not issued by this bridge"))
			}))
			defer srv.Close()
			e := NewCursorExecutor(&config.Config{})
			auth := &cliproxyauth.Auth{ID: "tA59", Attributes: map[string]string{"api_key": "k", "composer_client_tools_bridge_url": srv.URL}}
			req := cliproxyexecutor.Request{Model: "composer-2.5", Payload: []byte(`{"model":"composer-2.5","messages":[{"role":"user","content":"hi"}]}`)}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai"), Headers: http.Header{"X-Conversation-Id": []string{"a59-" + mode}}}
			var err error
			if mode == "stream" {
				_, err = e.executeComposerStream(context.Background(), auth, "k", req, opts)
			} else {
				_, err = e.executeComposer(context.Background(), auth, "k", req, opts)
			}
			if err == nil {
				t.Fatalf("ADD-59: a 410 from the bridge must error, never succeed")
			}
			var se cliproxyexecutor.StatusError
			if !errors.As(err, &se) {
				t.Fatalf("ADD-59: error must implement StatusError, got %T: %v", err, err)
			}
			if se.StatusCode() != 410 {
				t.Fatalf("ADD-59: status must be preserved as 410, got %d", se.StatusCode())
			}
			// The 410 message must NOT echo the raw bridge body (M25) but MUST be specific about the lost
			// continuation (re-seed/restart, not retry-later).
			if strings.Contains(err.Error(), "unknown or expired session: the tool") {
				t.Fatalf("ADD-59/M25: the raw bridge body must not leak into the client error: %v", err)
			}
			low := strings.ToLower(err.Error())
			if !strings.Contains(low, "re-seed") && !strings.Contains(low, "restart") {
				t.Fatalf("ADD-59: 410 message must signal a re-seedable/restartable continuation, got %v", err)
			}
		})
	}
}

// ADD-59: a non-410 non-2xx (e.g. 429) must also surface its status, distinct from the 410 lost-continuation
// wording so the client can tell "retry later" from "the continuation is gone".
func TestADD59_BridgeStatusErrorNon410(t *testing.T) {
	se := &composerBridgeStatusError{status: 429, correlation: "abc123"}
	if se.StatusCode() != 429 {
		t.Fatalf("status not preserved: %d", se.StatusCode())
	}
	if strings.Contains(strings.ToLower(se.Error()), "re-seed") {
		t.Fatalf("a 429 must not use the 410 lost-continuation wording: %v", se)
	}
	if !strings.Contains(se.Error(), "abc123") {
		t.Fatalf("correlation id must be in the message: %v", se)
	}
}

// ADD-41: a non-local bridge URL over plain HTTP must be REJECTED at request-build time (credential leak),
// while loopback http and any https are allowed. The opt-in env flag relaxes it.
func TestADD41_RequireHTTPSForRemoteBridge(t *testing.T) {
	cases := []struct {
		url      string
		insecure bool
		wantErr  bool
	}{
		{"http://127.0.0.1:9798", false, false},      // loopback http ok
		{"http://localhost:9798", false, false},      // loopback http ok
		{"https://bridge.example.com", false, false}, // remote https ok
		{"http://bridge.example.com", false, true},   // remote http rejected
		{"http://bridge.example.com", true, false},   // remote http allowed with flag
		{"http://169.254.10.1:9798", false, false},   // link-local http ok
	}
	for _, c := range cases {
		auth := &cliproxyauth.Auth{Attributes: map[string]string{"composer_client_tools_bridge_url": c.url}}
		if c.insecure {
			auth.Attributes["composer_client_tools_allow_insecure_bridge"] = "1"
		}
		_, err := buildComposerTurnURL(auth)
		if c.wantErr && err == nil {
			t.Fatalf("ADD-41: %q (insecure=%v) must be rejected", c.url, c.insecure)
		}
		if !c.wantErr && err != nil {
			t.Fatalf("ADD-41: %q (insecure=%v) must be allowed, got %v", c.url, c.insecure, err)
		}
	}
}

// ADD-47: the /agent/turn URL must be joined STRUCTURALLY (preserve a base path, drop a stray query) and a
// base with userinfo or a query string must be rejected (credentials must not ride in the URL).
func TestADD47_StructuralURLJoin(t *testing.T) {
	// Base path preserved, /agent/turn appended.
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"composer_client_tools_bridge_url": "https://bridge.example.com/cursor"}}
	got, err := buildComposerTurnURL(auth)
	if err != nil {
		t.Fatalf("structural join must succeed: %v", err)
	}
	if got != "https://bridge.example.com/cursor/agent/turn" {
		t.Fatalf("ADD-47: base path not preserved on join: %q", got)
	}
	// A query string in the base is rejected (would be mis-joined / could carry credentials).
	authQ := &cliproxyauth.Auth{Attributes: map[string]string{"composer_client_tools_bridge_url": "https://bridge.example.com?token=abc"}}
	if _, err := buildComposerTurnURL(authQ); err == nil {
		t.Fatalf("ADD-47: a base URL with a query string must be rejected")
	}
	// Userinfo in the base is rejected.
	authU := &cliproxyauth.Auth{Attributes: map[string]string{"composer_client_tools_bridge_url": "https://user:pass@bridge.example.com"}}
	if _, err := buildComposerTurnURL(authU); err == nil {
		t.Fatalf("ADD-47: a base URL with userinfo must be rejected")
	}
	// Default (loopback) still works and yields the canonical path.
	def, err := buildComposerTurnURL(&cliproxyauth.Auth{})
	if err != nil || !strings.HasSuffix(def, "/agent/turn") {
		t.Fatalf("ADD-47: default bridge URL must join to /agent/turn, got %q err=%v", def, err)
	}
}

// ADD-68: the SSE scanner buffer must be raised to align with the 64 MB body cap so a single large data:
// line does not tear down the stream. We assert the bound constant and that a >52 MB single line is read.
func TestADD68_ScannerBufferRaised(t *testing.T) {
	if composerSSEMaxLineBytes < 64<<20 {
		t.Fatalf("ADD-68: SSE line cap must be >= 64MB, got %d", composerSSEMaxLineBytes)
	}
	// Drive a single text delta larger than the old 52MB bound through the non-stream path; it must be read,
	// not fail with bufio.ErrTooLong.
	big := strings.Repeat("x", 53<<20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl, _ := w.(http.Flusher)
		ev, _ := json.Marshal(map[string]any{"type": "text", "delta": big})
		_, _ = w.Write([]byte("data: "))
		_, _ = w.Write(ev)
		_, _ = w.Write([]byte("\n\n"))
		if fl != nil {
			fl.Flush()
		}
		_, _ = w.Write([]byte("data: {\"type\":\"turn_end\",\"stop_reason\":\"stop\"}\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()
	e := NewCursorExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{ID: "tA68", Attributes: map[string]string{"api_key": "k", "composer_client_tools_bridge_url": srv.URL}}
	req := cliproxyexecutor.Request{Model: "composer-2.5", Payload: []byte(`{"model":"composer-2.5","messages":[{"role":"user","content":"hi"}]}`)}
	resp, err := e.executeComposer(context.Background(), auth, "k", req,
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai"), Headers: http.Header{"X-Conversation-Id": []string{"a68"}}})
	if err != nil {
		t.Fatalf("ADD-68: a >52MB single SSE line must not tear down the stream: %v", err)
	}
	if !strings.Contains(string(resp.Payload), "xxxx") {
		t.Fatalf("ADD-68: the large text delta was not delivered")
	}
}

// ADD-72: sampling/candidate controls (temperature/top_p/n) have no surface on the composer path, so they
// must be surfaced as explicit unsupportedHardGuarantees — never silently dropped, never faked.
func TestADD72_SamplingControlsUnsupported(t *testing.T) {
	c := composerConstraints([]byte(`{"temperature":0,"top_p":0.5,"n":3,"messages":[]}`))
	notes, _ := c["unsupportedHardGuarantees"].([]string)
	joined := strings.Join(notes, " | ")
	for _, want := range []string{"temperature", "top_p", "n="} {
		if !strings.Contains(joined, want) {
			t.Fatalf("ADD-72: %q sampling control not surfaced as unsupported: %q", want, joined)
		}
	}
	// n must NOT be carried as a constraint the bridge would silently ignore (no fake enforcement).
	if _, ok := c["n"]; ok {
		t.Fatalf("ADD-72: n must not be carried to the bridge (no fake enforcement)")
	}
	if _, ok := c["temperature"]; ok {
		t.Fatalf("ADD-72: temperature must not be carried to the bridge (no fake enforcement)")
	}
	// n=1 is the default — not worth a note.
	c1 := composerConstraints([]byte(`{"n":1,"messages":[]}`))
	if notes1, _ := c1["unsupportedHardGuarantees"].([]string); strings.Contains(strings.Join(notes1, " "), "n=") {
		t.Fatalf("ADD-72: n=1 must not be flagged: %v", notes1)
	}
}

// ADD-67: prior assistant reasoning must NOT be replayed verbatim as plain prompt text on a re-seed (default);
// it becomes a neutral omission marker. The EX9 behavior is restorable behind a compat flag.
func TestADD67_ReasoningNotReplayedByDefault(t *testing.T) {
	messages := gjson.GetBytes([]byte(`{"messages":[
		{"role":"user","content":"q"},
		{"role":"assistant","content":"answer","reasoning_content":"SECRET chain of thought"},
		{"role":"user","content":"next"}
	]}`), "messages").Array()
	h := renderComposerHistory(messages, 2)
	if strings.Contains(h, "SECRET chain of thought") {
		t.Fatalf("ADD-67: raw reasoning must not be replayed as prompt text: %q", h)
	}
	if !strings.Contains(h, "[assistant reasoning omitted]") {
		t.Fatalf("ADD-67: an omission marker must replace replayed reasoning: %q", h)
	}
	if !strings.Contains(h, "ASSISTANT: answer") {
		t.Fatalf("ADD-67: the assistant answer text must still be present: %q", h)
	}
	// Compat flag restores verbatim replay.
	t.Setenv("CURSOR_COMPOSER_REPLAY_REASONING", "1")
	composerReplayReasoningEnabled = true
	defer func() { composerReplayReasoningEnabled = false }()
	h2 := renderComposerHistory(messages, 2)
	if !strings.Contains(h2, "thinking: SECRET chain of thought") {
		t.Fatalf("ADD-67: compat flag must restore verbatim reasoning replay: %q", h2)
	}
}

// ADD-56: an image-only turn whose every image is degenerate must be rejected with a typed 400 (not a silent
// empty turn). A mixed text+invalid-image turn must keep a model-visible invalid-attachment placeholder.
func TestADD56_DegenerateImageOnlyTurn(t *testing.T) {
	// image-only + all-invalid => rejected.
	imgOnly := gjson.GetBytes([]byte(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:,"}}]}]}`), "messages").Array()
	if !lastUserTurnImageOnlyInvalid(imgOnly, composerContinuationHint{}) {
		t.Fatalf("ADD-56: an image-only turn with only a degenerate image must be flagged invalid")
	}
	// mixed text + invalid image => NOT rejected, placeholder added in composerInput.
	mixed := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"what is this"},{"type":"image_url","image_url":{"url":"data:,"}}]}]}`)
	if lastUserTurnImageOnlyInvalid(gjson.GetBytes(mixed, "messages").Array(), composerContinuationHint{}) {
		t.Fatalf("ADD-56: a mixed text+invalid-image turn must NOT be rejected (text is present)")
	}
	inp := composerInput(mixed)
	text, _ := inp["text"].(string)
	if !strings.Contains(text, "what is this") || !strings.Contains(text, "could not be processed") {
		t.Fatalf("ADD-56: a mixed turn must keep the text AND a model-visible invalid-image placeholder, got %q", text)
	}
	// a valid image-only turn is fine.
	validOnly := gjson.GetBytes([]byte(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,QQ=="}}]}]}`), "messages").Array()
	if lastUserTurnImageOnlyInvalid(validOnly, composerContinuationHint{}) {
		t.Fatalf("ADD-56: a valid image-only turn must NOT be flagged invalid")
	}
	// a plain text-only turn is fine (no image parts).
	textOnly := gjson.GetBytes([]byte(`{"messages":[{"role":"user","content":"hello"}]}`), "messages").Array()
	if lastUserTurnImageOnlyInvalid(textOnly, composerContinuationHint{}) {
		t.Fatalf("ADD-56: a text-only turn must NOT be flagged invalid")
	}
}

// ADD-56: the executor must reject an image-only-all-invalid turn with a typed 400, both stream and nonstream.
func TestADD56_ExecutorRejectsImageOnlyInvalid(t *testing.T) {
	// The bridge must never be called; fail the test if it is.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("ADD-56: the bridge must not be called for an invalid image-only turn")
		w.WriteHeader(200)
	}))
	defer srv.Close()
	e := NewCursorExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{ID: "tA56", Attributes: map[string]string{"api_key": "k", "composer_client_tools_bridge_url": srv.URL}}
	payload := []byte(`{"model":"composer-2.5","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:,"}}]}]}`)
	req := cliproxyexecutor.Request{Model: "composer-2.5", Payload: payload}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai"), Headers: http.Header{"X-Conversation-Id": []string{"a56x"}}}
	_, err := e.executeComposer(context.Background(), auth, "k", req, opts)
	if err == nil {
		t.Fatalf("ADD-56: nonstream must reject an image-only-invalid turn")
	}
	var se cliproxyexecutor.StatusError
	if !errors.As(err, &se) || se.StatusCode() != http.StatusBadRequest {
		t.Fatalf("ADD-56: must be a typed 400, got %T %v", err, err)
	}
	if _, err := e.executeComposerStream(context.Background(), auth, "k", req, opts); err == nil {
		t.Fatalf("ADD-56: stream must reject an image-only-invalid turn")
	}
}

// ADD-55: extractComposerimages must accept both the object form image_url:{url} AND the scalar form
// image_url:"…"; a file_id-only part has no url and is treated as degenerate (not an empty image part).
func TestADD55_ImageURLScalarAndObjectForms(t *testing.T) {
	obj := gjson.Parse(`{"content":[{"type":"image_url","image_url":{"url":"https://x/y.png"}}]}`)
	if imgs := extractComposerImages(obj); len(imgs) != 1 || imgs[0]["url"] != "https://x/y.png" {
		t.Fatalf("ADD-55: object-form image_url not extracted: %v", imgs)
	}
	scalar := gjson.Parse(`{"content":[{"type":"image_url","image_url":"https://x/z.jpg"}]}`)
	if imgs := extractComposerImages(scalar); len(imgs) != 1 || imgs[0]["url"] != "https://x/z.jpg" {
		t.Fatalf("ADD-55: scalar-form image_url not extracted: %v", imgs)
	}
	// file_id-only part: no fetchable url -> degenerate (skipped), never an empty-url image part.
	fileID := gjson.Parse(`{"content":[{"type":"input_image","file_id":"file_123"}]}`)
	if imgs := extractComposerImages(fileID); len(imgs) != 0 {
		t.Fatalf("ADD-55: a file_id-only image must not yield an (empty) image part: %v", imgs)
	}
	if !messageHasImageParts(fileID) {
		t.Fatalf("ADD-55: a file_id image part must be detected as an image part (so ADD-56 can flag it)")
	}
}

// ADD-65: a Responses server-side-chained continuation [..., role:tool, role:user] with previous_response_id
// (and NO assistant tool_calls adjacency) must classify as a tool_results continuation with userText, never a
// fresh user turn.
func TestADD65_ResponsesChainedContinuationIsToolResults(t *testing.T) {
	// previous_response_id present => branch (c) via the hint.
	oai := []byte(`{"previous_response_id":"resp_x","messages":[
		{"role":"user","content":"do the thing"},
		{"role":"tool","tool_call_id":"call_A","content":"tool said hi"},
		{"role":"user","content":"Also do X"}
	]}`)
	inp := composerInput(oai)
	if inp["type"] != "tool_results" {
		t.Fatalf("ADD-65: chained continuation must be tool_results, got %v", inp["type"])
	}
	if inp["userText"] != "Also do X" {
		t.Fatalf("ADD-65: the trailing user text must be carried as userText, got %v", inp["userText"])
	}
	results, _ := inp["results"].([]map[string]any)
	if len(results) != 1 || results[0]["toolCallId"] != "call_A" {
		t.Fatalf("ADD-65: the tool result must be collected, got %v", results)
	}
	// Without previous_response_id AND without tool ownership, the SAME shape must NOT be a continuation (it is
	// an ordinary conversation that ends [tool, user]) — it falls through to a fresh user turn.
	plain := []byte(`{"messages":[
		{"role":"user","content":"do the thing"},
		{"role":"tool","tool_call_id":"call_unowned","content":"tool said hi"},
		{"role":"user","content":"Also do X"}
	]}`)
	if inp2 := composerInput(plain); inp2["type"] != "user" {
		t.Fatalf("ADD-65: without a continuation signal the [tool,user] shape must be a fresh user turn, got %v", inp2["type"])
	}
}

// ADD-65: ownership alone (no previous_response_id) is a valid continuation signal — when a trailing tool id
// is owned for the tenant, the chained shape classifies as tool_results and routes to the owning session.
func TestADD65_OwnershipSignalRoutesContinuation(t *testing.T) {
	auth := authWith("authA65", "keyA65")
	owner := "sess_owner65000000000000000000000000"
	tenant := composerTenant(auth, cliproxyexecutor.Options{})
	recordComposerToolCall(tenant, "call_owned65", owner)
	oai := []byte(`{"messages":[
		{"role":"user","content":"start"},
		{"role":"tool","tool_call_id":"call_owned65","content":"R"},
		{"role":"user","content":"and more"}
	]}`)
	inp := composerInputHinted(oai, composerContinuationHintFor(tenant, oai))
	if inp["type"] != "tool_results" {
		t.Fatalf("ADD-65: an owned trailing tool id must make the chained shape a tool_results continuation, got %v", inp["type"])
	}
	got, err := deriveComposerSessionID(auth, "cursorkey", oai, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("ADD-65: routing must not error: %v", err)
	}
	if got != owner {
		t.Fatalf("ADD-65: chained continuation must route to the owning session %s, got %s", owner, got)
	}
}

// ADD-70: the same visible tool-call id emitted by TWO of a tenant's sessions must be AMBIGUOUS — a
// continuation echoing it with no stable id must surface a typed error, never silently pick the latest writer.
func TestADD70_AmbiguousOwnershipRejectedWithoutDisambiguator(t *testing.T) {
	auth := authWith("authA70", "keyA70")
	tenant := composerTenant(auth, cliproxyexecutor.Options{})
	recordComposerToolCall(tenant, "call_dup70", "sess_a70000000000000000000000000000")
	recordComposerToolCall(tenant, "call_dup70", "sess_b70000000000000000000000000000")
	cont := []byte(`{"messages":[{"role":"assistant","tool_calls":[{"id":"call_dup70"}]},{"role":"tool","tool_call_id":"call_dup70","content":"R"}]}`)
	// No stable id and no previous_response_id -> ambiguous -> typed error.
	_, err := deriveComposerSessionID(auth, "cursorkey", cont, cliproxyexecutor.Options{})
	if !errors.Is(err, errAmbiguousToolOwnership) {
		t.Fatalf("ADD-70: an ambiguous id with no disambiguator must return errAmbiguousToolOwnership, got %v", err)
	}
	// With a stable conv id, it disambiguates by routing to the conv-id session (no error).
	got, err2 := deriveComposerSessionID(auth, "cursorkey", cont, optsWithHeaders(map[string]string{"X-Conversation-Id": "conv-disambig70"}))
	if err2 != nil {
		t.Fatalf("ADD-70: a stable conv id must disambiguate an ambiguous ownership, got %v", err2)
	}
	if !strings.HasPrefix(got, "sess_") {
		t.Fatalf("ADD-70: disambiguated route must be a sess_ id, got %q", got)
	}
}

// ADD-50: re-emitting an active tool-call id must perform a true LRU touch (keep it), so unrelated churn does
// not evict it. We use a tiny per-tenant cap via a fresh store to prove the touched key survives.
func TestADD50_TrueLRURefresh(t *testing.T) {
	s := newComposerToolCallStore()
	s.perCap = 3
	s.entryTTL = 0 // disable TTL for this test
	tenant := "tnt50"
	s.record(tenant, "id_keep", "sess_keep")
	s.record(tenant, "id_1", "sess_x")
	s.record(tenant, "id_2", "sess_x")
	// Touch id_keep so it becomes most-recently-used; then push two more to evict the oldest.
	s.record(tenant, "id_keep", "sess_keep") // touch (same session)
	s.record(tenant, "id_3", "sess_x")       // evicts the now-oldest (id_1)
	s.record(tenant, "id_4", "sess_x")       // evicts id_2
	to := s.tenants[tenant]
	if to == nil {
		t.Fatalf("ADD-50: tenant ownership missing")
	}
	if _, ok := to.byKey[composerOwnershipKey("sess_keep", "id_keep")]; !ok {
		t.Fatalf("ADD-50: a touched (re-emitted) id must survive cap eviction; it was evicted")
	}
	if _, ok := to.byKey[composerOwnershipKey("sess_x", "id_1")]; ok {
		t.Fatalf("ADD-50: the genuinely-oldest id_1 should have been evicted")
	}
}

// ADD-71: per-tenant cap — one noisy tenant must NOT evict another tenant's ownership entries.
func TestADD71_PerTenantCapIsolation(t *testing.T) {
	s := newComposerToolCallStore()
	s.perCap = 2
	s.entryTTL = 0
	// Quiet tenant records one id.
	s.record("quiet", "q_id", "sess_q")
	// Noisy tenant blows way past its own cap.
	for i := 0; i < 50; i++ {
		s.record("noisy", fmt.Sprintf("n_%d", i), "sess_n")
	}
	if to := s.tenants["quiet"]; to == nil || len(to.byTool["q_id"]) == 0 {
		t.Fatalf("ADD-71: a noisy tenant must not evict the quiet tenant's entry")
	}
	if to := s.tenants["noisy"]; to == nil || len(to.byKey) > s.perCap {
		t.Fatalf("ADD-71: the noisy tenant must be bounded by its own per-tenant cap")
	}
}

// ADD-71: ownership entries expire by age (TTL), so the cap is not the only cleanup; and forgetSession drops a
// finished session's entries.
func TestADD71_TTLAndSessionForget(t *testing.T) {
	s := newComposerToolCallStore()
	now := time.Now()
	s.nowFn = func() time.Time { return now }
	s.entryTTL = time.Minute
	s.record("tnt", "old_id", "sess_old")
	// Advance past TTL; a lookup-time expiry must drop it.
	now = now.Add(2 * time.Minute)
	if s.ownsTool("tnt", "old_id") {
		t.Fatalf("ADD-71: an entry older than the TTL must be expired")
	}
	// forgetSession drops a session's entries explicitly.
	now = time.Now()
	s.nowFn = func() time.Time { return now }
	s.record("tnt2", "a", "sess_keep")
	s.record("tnt2", "b", "sess_drop")
	s.forgetSession("tnt2", "sess_drop")
	if s.ownsTool("tnt2", "b") {
		t.Fatalf("ADD-71: forgetSession must drop the finished session's id")
	}
	if !s.ownsTool("tnt2", "a") {
		t.Fatalf("ADD-71: forgetSession must keep other sessions' ids")
	}
}

// ADD-71: the exported forgetComposerSessionToolCalls wrapper drops a session's entries from the global store.
func TestADD71_ForgetWrapper(t *testing.T) {
	tenant := "tnt_forget_wrapper"
	recordComposerToolCall(tenant, "fw_id", "sess_fw")
	if !composerOwnership.ownsTool(tenant, "fw_id") {
		t.Fatalf("ADD-71: setup — id must be owned before forget")
	}
	forgetComposerSessionToolCalls(tenant, "sess_fw")
	if composerOwnership.ownsTool(tenant, "fw_id") {
		t.Fatalf("ADD-71: forgetComposerSessionToolCalls must drop the session's id from the global store")
	}
}

// ADD-62: the external session id must be derived from tenant+conversation ONLY, never the model, so a model
// change on the same conversation keeps the same session id (the bridge then rotates the durable agent).
func TestADD62_SessionIDStableAcrossModelChange(t *testing.T) {
	auth := authWith("authA62", "keyA62")
	conv := optsWithHeaders(map[string]string{"X-Conversation-Id": "conv-62"})
	// Two turns on the same conversation with DIFFERENT models must derive the SAME external session id.
	turnFast := []byte(`{"model":"composer-2.5-fast","tools":[{"type":"function","function":{"name":"Read"}}],"messages":[{"role":"user","content":"hi"}]}`)
	turnStd := []byte(`{"model":"composer-2.5","tools":[{"type":"function","function":{"name":"Read"}}],"messages":[{"role":"user","content":"hi"}]}`)
	s1, err1 := deriveComposerSessionID(auth, "cursorkey", turnFast, conv)
	s2, err2 := deriveComposerSessionID(auth, "cursorkey", turnStd, conv)
	if err1 != nil || err2 != nil {
		t.Fatalf("ADD-62: routing must not error: %v / %v", err1, err2)
	}
	if s1 != s2 {
		t.Fatalf("ADD-62: a model change on the same conversation must keep the same session id: %s vs %s", s1, s2)
	}
}

// ADD-36 (executor side): a partial-parallel turn — the model emitted tools A,B,C; the client returns ONLY
// A's result and the user types new text in the same turn — must classify as a tool_results continuation and
// carry the trailing user text as first-class userText (so the bridge can drive the interruption / answer it),
// never as a fresh user turn that strands the result. The unresolved B/C are the bridge's concern.
func TestADD36_PartialParallelToolResultsCarriesUserText(t *testing.T) {
	mixed := composerInput([]byte(`{"messages":[
		{"role":"user","content":"do A B C"},
		{"role":"assistant","content":"","tool_calls":[
			{"id":"tc_A","function":{"name":"Read"}},
			{"id":"tc_B","function":{"name":"Read"}},
			{"id":"tc_C","function":{"name":"Read"}}
		]},
		{"role":"tool","tool_call_id":"tc_A","content":"A done"},
		{"role":"user","content":"actually, stop and summarize"}
	]}`))
	if mixed["type"] != "tool_results" {
		t.Fatalf("ADD-36: a partial-parallel mixed turn must be tool_results, got %v", mixed["type"])
	}
	if mixed["userText"] != "actually, stop and summarize" {
		t.Fatalf("ADD-36: the trailing user message must be carried as first-class userText, got %v", mixed["userText"])
	}
	results, _ := mixed["results"].([]map[string]any)
	if len(results) != 1 || results[0]["toolCallId"] != "tc_A" {
		t.Fatalf("ADD-36: only the returned tool result (A) is present; the bridge owns B/C, got %v", results)
	}
	// The trailing text is also folded into A's content so a resuming run reads it inline.
	if c, _ := results[0]["content"].(string); !strings.Contains(c, "A done") || !strings.Contains(c, "stop and summarize") {
		t.Fatalf("ADD-36: trailing user text must also be folded into the last result content, got %q", c)
	}
}

// ADD-78: a request whose ONLY identity signal is prompt_cache_key must MINT a fresh session (cache key is a
// locality hint, not a conversation identity), not hash to a stable sess_ id — otherwise independent tasks
// sharing a coarse cache key merge onto one durable Cursor agent.
func TestADD78_PromptCacheKeyNotStable(t *testing.T) {
	auth := authWith("authA", "keyA")
	o := cliproxyexecutor.Options{OriginalRequest: []byte(`{"prompt_cache_key":"repoX-cache"}`)}
	// stableConversationID must NOT return the prompt_cache_key.
	if id := stableConversationID(o); id != "" {
		t.Fatalf("ADD-78: prompt_cache_key must not be a stable conversation id, got %q", id)
	}
	// Two turns with the SAME prompt_cache_key but no other id must get DISTINCT minted sessions (proves it is
	// not hashed to a stable id). BRANCH=mint.
	s1, err := deriveComposerSessionID(auth, "cursorkey", toolTurn("task one"), o)
	if err != nil || !strings.HasPrefix(s1, "sess_") {
		t.Fatalf("ADD-78: a prompt_cache_key-only turn must route (mint), got id=%q err=%v", s1, err)
	}
	s2, _ := deriveComposerSessionID(auth, "cursorkey", toolTurn("task two"), o)
	if s1 == s2 {
		t.Fatalf("ADD-78: prompt_cache_key must NOT mint a stable session (distinct tasks must not merge): %s == %s", s1, s2)
	}
	// A real conversation_id is still stable (regression guard for the surviving signal).
	convOpts := cliproxyexecutor.Options{OriginalRequest: []byte(`{"conversation_id":"conv-real"}`)}
	c1, _ := deriveComposerSessionID(auth, "cursorkey", toolTurn("a"), convOpts)
	c2, _ := deriveComposerSessionID(auth, "cursorkey", toolTurn("b"), convOpts)
	if c1 != c2 {
		t.Fatalf("ADD-78: conversation_id must remain stable, got %s vs %s", c1, c2)
	}
}

// ADD-79 (exec half): the same conversation id under a DIFFERENT upstream Cursor key must derive a DIFFERENT
// sess_ id (a key rotation re-routes to a fresh durable agent), while the same key is stable.
func TestADD79_KeyFingerprintFoldedIntoSession(t *testing.T) {
	auth := authWith("authA", "keyA")
	conv := optsWithHeaders(map[string]string{"X-Conversation-Id": "conv-key-rot"})
	a, err := deriveComposerSessionID(auth, "cursor-key-one", toolTurn("hi"), conv)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	// Same conv + same key => stable.
	aAgain, _ := deriveComposerSessionID(auth, "cursor-key-one", toolTurn("different text"), conv)
	if a != aAgain {
		t.Fatalf("ADD-79: same conv + same key must be stable, got %s vs %s", a, aAgain)
	}
	// Same conv + DIFFERENT key => different session (the rotated key re-routes).
	b, _ := deriveComposerSessionID(auth, "cursor-key-two", toolTurn("hi"), conv)
	if a == b {
		t.Fatalf("ADD-79: a rotated Cursor key must yield a different sess_ id for the same conversation (a=%s b=%s)", a, b)
	}
	// The fingerprint helper is non-reversible and stable, and empty for an empty key.
	if composerKeyFingerprint("") != "" {
		t.Fatalf("ADD-79: empty key must yield an empty fingerprint")
	}
	if fp := composerKeyFingerprint("cursor-key-one"); len(fp) != 16 || strings.Contains(fp, "cursor-key-one") {
		t.Fatalf("ADD-79: fingerprint must be a 16-hex non-reversible digest, got %q", fp)
	}
}

// ADD-83/87 (exec half): reasoning_effort is surfaced SOLELY via unsupportedHardGuarantees (the SDK has no
// reasoning-effort knob and the bridge does not read a reasoningEffort key, so a dedicated body field would be
// dead). The failure path asserted here is the dead-key regression: a reasoningEffort field must NOT be set.
func TestADD83_ReasoningEffortFlaggedNotCarriedAsDeadKey(t *testing.T) {
	c := composerConstraints([]byte(`{"reasoning_effort":"high"}`))
	// Regression guard: the bridge's constraints allowlist never reads reasoningEffort, so writing it back
	// would be a dead wire field. It must be absent — the signal lives only in unsupportedHardGuarantees.
	if _, ok := c["reasoningEffort"]; ok {
		t.Fatalf("ADD-87: reasoningEffort must NOT be carried as a dedicated (dead) body field, got %#v", c["reasoningEffort"])
	}
	notes, _ := c["unsupportedHardGuarantees"].([]string)
	if !strings.Contains(strings.Join(notes, " | "), "reasoning_effort=high") {
		t.Fatalf("ADD-87: reasoning_effort must be flagged unsupported, got %#v", notes)
	}
	// Absent reasoning_effort => no note at all.
	if _, ok := composerConstraints([]byte(`{}`))["unsupportedHardGuarantees"]; ok {
		t.Fatalf("ADD-87: no reasoning_effort must not add an unsupported note")
	}
}

// ADD-83 (exec half): a tool_results CONTINUATION turn must carry the bridge-CONSUMED constraints
// (responseFormat/stop/maxTokens) plus system to the bridge body (composerInputHinted attaches system;
// composerTurnBody spreads constraints) so the bridge can apply them on the resume. reasoning_effort is NOT a
// bridge-consumed field — it is surfaced only via unsupportedHardGuarantees — so it must NOT appear as a dead
// reasoningEffort body key (the dead-key regression guard below).
func TestADD83_ContinuationBodyCarriesConstraintsAndSystem(t *testing.T) {
	oai := []byte(`{
		"response_format":{"type":"json_object"},
		"stop":["STOP"],
		"max_tokens":256,
		"reasoning_effort":"medium",
		"messages":[
			{"role":"system","content":"NEW SYSTEM"},
			{"role":"assistant","tool_calls":[{"id":"tc_1","function":{"name":"Read"}}]},
			{"role":"tool","tool_call_id":"tc_1","content":"RESULT"}
		]}`)
	inp := composerInput(oai)
	if inp["type"] != "tool_results" {
		t.Fatalf("ADD-83: expected a tool_results continuation, got %v", inp["type"])
	}
	if inp["system"] != "NEW SYSTEM" {
		t.Fatalf("ADD-83: continuation input must carry the (possibly swapped) system, got %v", inp["system"])
	}
	body := composerTurnBody("s1", "composer-2.5", inp, nil, "", nil, composerConstraints(oai))
	if gjson.GetBytes(body, "input.system").String() != "NEW SYSTEM" {
		t.Fatalf("ADD-83: bridge body must carry input.system on a continuation, got %s", body)
	}
	if gjson.GetBytes(body, "responseFormat.type").String() != "json_object" {
		t.Fatalf("ADD-83: continuation body must carry responseFormat, got %s", body)
	}
	if gjson.GetBytes(body, "stop.0").String() != "STOP" || gjson.GetBytes(body, "maxTokens").Int() != 256 {
		t.Fatalf("ADD-83: continuation body must carry stop/maxTokens, got %s", body)
	}
	// Dead-key regression guard: reasoning_effort is bridge-advisory only (unsupportedHardGuarantees), so the
	// continuation body must NOT carry a reasoningEffort field the bridge would never read.
	if gjson.GetBytes(body, "reasoningEffort").Exists() {
		t.Fatalf("ADD-83: continuation body must NOT carry a dead reasoningEffort field, got %s", body)
	}
	// The advisory note for reasoning_effort must still reach the bridge (so the model is told it is best-effort).
	if !strings.Contains(gjson.GetBytes(body, "unsupportedHardGuarantees").Raw, "reasoning_effort=medium") {
		t.Fatalf("ADD-83: continuation body must carry the reasoning_effort advisory in unsupportedHardGuarantees, got %s", body)
	}
}

// ADD-89 (exec half): a tool_results turn whose ONLY result is isError:true + trailing user text must NOT
// fold the instruction into the failed result's content; the user text must remain first-class userText.
func TestADD89_TrailingTextNotFoldedIntoErrorResult(t *testing.T) {
	mixed := composerInput([]byte(`{"messages":[
		{"role":"assistant","tool_calls":[{"id":"tc_1","function":{"name":"Bash"}}]},
		{"role":"tool","tool_call_id":"tc_1","is_error":true,"content":"Bash was cancelled"},
		{"role":"user","content":"Ignore that failed command and just explain what to do next."}
	]}`))
	if mixed["type"] != "tool_results" {
		t.Fatalf("ADD-89: expected tool_results, got %v", mixed["type"])
	}
	results, _ := mixed["results"].([]map[string]any)
	if len(results) != 1 {
		t.Fatalf("ADD-89: expected 1 result, got %d", len(results))
	}
	content, _ := results[0]["content"].(string)
	// The failed result's content must NOT be mutated to embed the instruction.
	if content != "Bash was cancelled" {
		t.Fatalf("ADD-89: the user instruction must NOT be folded into a failed (isError) result, got %q", content)
	}
	if isErr, _ := results[0]["isError"].(bool); !isErr {
		t.Fatalf("ADD-89: the error flag must be preserved")
	}
	// The trailing text must still be carried first-class so the bridge can answer it.
	if mixed["userText"] != "Ignore that failed command and just explain what to do next." {
		t.Fatalf("ADD-89: trailing user text must remain first-class userText, got %v", mixed["userText"])
	}

	// Control: when a NON-error result is also present, the text folds into the LAST NON-ERROR result, not the error one.
	mixed2 := composerInput([]byte(`{"messages":[
		{"role":"assistant","tool_calls":[{"id":"tc_ok","function":{"name":"Read"}},{"id":"tc_err","function":{"name":"Bash"}}]},
		{"role":"tool","tool_call_id":"tc_ok","content":"OK OUTPUT"},
		{"role":"tool","tool_call_id":"tc_err","is_error":true,"content":"failed"},
		{"role":"user","content":"now summarize"}
	]}`))
	res2, _ := mixed2["results"].([]map[string]any)
	if len(res2) != 2 {
		t.Fatalf("ADD-89: expected 2 results, got %d", len(res2))
	}
	if c, _ := res2[0]["content"].(string); !strings.Contains(c, "OK OUTPUT") || !strings.Contains(c, "now summarize") {
		t.Fatalf("ADD-89: trailing text must fold into the last NON-error result, got %q", c)
	}
	if c, _ := res2[1]["content"].(string); c != "failed" {
		t.Fatalf("ADD-89: the error result must remain unmodified, got %q", c)
	}
}

// ADD-95 (exec half): an oversized LIVE tool-result is capped and carries the 'truncated by proxy' marker;
// a small result is unchanged.
func TestADD95_LiveToolResultTruncated(t *testing.T) {
	small := "short result"
	if got := truncateCursorToolResultLive(small); got != small {
		t.Fatalf("ADD-95: a small result must be unchanged, got %q", got)
	}
	big := strings.Repeat("x", composerLiveToolResultMaxBytes+5000)
	got := truncateCursorToolResultLive(big)
	if len(got) >= len(big) {
		t.Fatalf("ADD-95: an oversized result must be truncated (len %d >= original %d)", len(got), len(big))
	}
	if !strings.Contains(got, "truncated by proxy") {
		t.Fatalf("ADD-95: a truncated result must carry the 'truncated by proxy' marker")
	}
	// End-to-end: the cap is applied to the live tool result content in composerToolResultsHinted.
	bigJSON := strings.Repeat("y", composerLiveToolResultMaxBytes+1000)
	cont := composerInput([]byte(`{"messages":[
		{"role":"assistant","tool_calls":[{"id":"tc_1","function":{"name":"Read"}}]},
		{"role":"tool","tool_call_id":"tc_1","content":"` + bigJSON + `"}
	]}`))
	results, _ := cont["results"].([]map[string]any)
	if len(results) != 1 {
		t.Fatalf("ADD-95: expected 1 result, got %d", len(results))
	}
	c, _ := results[0]["content"].(string)
	if len(c) > composerLiveToolResultMaxBytes+200 || !strings.Contains(c, "truncated by proxy") {
		t.Fatalf("ADD-95: live tool-result content must be capped + marked (len=%d)", len(c))
	}
}

// ADD-99 (exec half): a tool with strict:true must carry strict on the advertise entry AND be flagged in
// composerConstraints as not-hard-validated; a non-strict tool carries neither.
func TestADD99_StrictHintPreservedAndFlagged(t *testing.T) {
	oai := []byte(`{"tools":[{"type":"function","function":{"name":"edit_file","strict":true,"parameters":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"],"additionalProperties":false}}}]}`)
	adv := composerAdvertise(oai)
	if len(adv) != 1 {
		t.Fatalf("ADD-99: expected 1 advertised tool, got %d", len(adv))
	}
	if strict, ok := adv[0]["strict"].(bool); !ok || !strict {
		t.Fatalf("ADD-99: advertise entry must carry strict:true, got %#v", adv[0]["strict"])
	}
	notes, _ := composerConstraints(oai)["unsupportedHardGuarantees"].([]string)
	joined := strings.Join(notes, " | ")
	if !strings.Contains(joined, "edit_file") || !strings.Contains(joined, "strict") {
		t.Fatalf("ADD-99: strict:true must be flagged as not-hard-validated, got %q", joined)
	}
	// A non-strict tool carries neither the strict key nor a strict note.
	plain := []byte(`{"tools":[{"type":"function","function":{"name":"Read","parameters":{"type":"object"}}}]}`)
	if _, ok := composerAdvertise(plain)[0]["strict"]; ok {
		t.Fatalf("ADD-99: a non-strict tool must NOT carry a strict key")
	}
	if n, _ := composerConstraints(plain)["unsupportedHardGuarantees"].([]string); strings.Contains(strings.Join(n, " | "), "strict") {
		t.Fatalf("ADD-99: a non-strict tool must NOT be flagged for strict args")
	}
}

// ADD-104 (exec half): a tool_call whose input is NOT an object (a JSON string or number) must preserve the
// raw value under {"input":<raw>} in the emitted arguments, never collapse to {}.
func TestADD104_NonObjectToolInputPreserved(t *testing.T) {
	defs := []cursorToolDefinition{{Name: "Read"}}
	// String input.
	_, argsStr := mapComposerToolCall("Read", gjson.Parse(`"hi there"`), defs, nil)
	if gjson.Get(argsStr, "input").String() != "hi there" {
		t.Fatalf("ADD-104: a string input must be preserved under {\"input\":...}, got %s", argsStr)
	}
	// Number input.
	_, argsNum := mapComposerToolCall("Read", gjson.Parse(`5`), defs, nil)
	if gjson.Get(argsNum, "input").Int() != 5 {
		t.Fatalf("ADD-104: a number input must be preserved under {\"input\":...}, got %s", argsNum)
	}
	// Array input.
	_, argsArr := mapComposerToolCall("Read", gjson.Parse(`[1,2,3]`), defs, nil)
	if !gjson.Get(argsArr, "input").IsArray() {
		t.Fatalf("ADD-104: an array input must be preserved under {\"input\":...}, got %s", argsArr)
	}
	// An unknown tool name with a string input still preserves the value (and passes the name through).
	name, argsUnknown := mapComposerToolCall("totally_unknown", gjson.Parse(`"raw"`), defs, nil)
	if name != "totally_unknown" || gjson.Get(argsUnknown, "input").String() != "raw" {
		t.Fatalf("ADD-104: unknown tool with string input must pass name + preserve value, got name=%q args=%s", name, argsUnknown)
	}
	// An OBJECT input is unchanged (no spurious wrapper).
	_, argsObj := mapComposerToolCall("Read", gjson.Parse(`{"path":"/x"}`), defs, nil)
	if gjson.Get(argsObj, "path").String() != "/x" || gjson.Get(argsObj, "input").Exists() {
		t.Fatalf("ADD-104: an object input must NOT be wrapped, got %s", argsObj)
	}
}

// Comment 5: a Responses previous_response_id continuation (and a tool-ownership-hinted continuation) whose
// trailing user image is malformed must NOT be rejected by lastUserTurnImageOnlyInvalid before tool results
// are handled — it is a continuation, not an image-only new-user turn.
func TestComment5_HintedContinuationNotRejectedAsImageOnly(t *testing.T) {
	// A Responses server-side-chained shape: [..., role:tool, role:user(malformed image only)] with NO
	// assistant tool_calls anchor — recognized as a continuation ONLY via the previous_response_id hint.
	msgs := gjson.GetBytes([]byte(`{"messages":[
		{"role":"tool","tool_call_id":"tc_resp","content":"file contents"},
		{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:,"}}]}
	]}`), "messages").Array()

	// WITHOUT the hint, branch (c) does not fire, so it would look like an image-only new-user turn and be
	// rejected — this is the bug Comment 5 fixes.
	if !lastUserTurnImageOnlyInvalid(msgs, composerContinuationHint{}) {
		t.Fatalf("Comment 5 (precondition): without a hint, the [tool,user-bad-image] shape looks image-only")
	}
	// WITH the previous_response_id hint, it is correctly recognized as a continuation and NOT rejected.
	prevHint := composerContinuationHint{hasPreviousResponseID: true}
	if lastUserTurnImageOnlyInvalid(msgs, prevHint) {
		t.Fatalf("Comment 5: a previous_response_id continuation with a malformed trailing image must NOT be rejected as image-only")
	}
	// WITH a tool-ownership hint (the trailing tool id is owned), it is likewise recognized as a continuation.
	ownHint := composerContinuationHint{ownsToolCallID: func(id string) bool { return id == "tc_resp" }}
	if lastUserTurnImageOnlyInvalid(msgs, ownHint) {
		t.Fatalf("Comment 5: a tool-ownership-hinted continuation with a malformed trailing image must NOT be rejected as image-only")
	}
}

func TestADD94_StoreFalseRejectedAsTyped400(t *testing.T) {
	// store:false must be rejected with a typed 4xx BEFORE any durable Cursor state is created — never silently
	// persisted (ADD-94 / Comment 4). The Responses translator surfaces store:false onto the normalized body.
	err := composerRejectStoreFalse([]byte(`{"model":"composer-2.5","messages":[],"store":false}`))
	if err == nil {
		t.Fatal("ADD-94: store:false must be rejected, not silently persisted")
	}
	sc, ok := err.(interface{ StatusCode() int })
	if !ok || sc.StatusCode() != http.StatusBadRequest {
		t.Fatalf("ADD-94: store:false must reject as a typed %d, got %v", http.StatusBadRequest, err)
	}
	// store:true and absent are the durable default — accepted (no reject).
	if errTrue := composerRejectStoreFalse([]byte(`{"store":true}`)); errTrue != nil {
		t.Fatalf("ADD-94: store:true must be accepted, got %v", errTrue)
	}
	if errAbsent := composerRejectStoreFalse([]byte(`{"model":"composer-2.5"}`)); errAbsent != nil {
		t.Fatalf("ADD-94: absent store must be accepted, got %v", errAbsent)
	}
}
