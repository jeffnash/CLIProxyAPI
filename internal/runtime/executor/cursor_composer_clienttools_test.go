package executor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
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
	hdr.Set("X-Cwd", "/tmp/cliproxy-test-workspace")
	hdr.Set("X-Workspace-Path", "/tmp/cliproxy-test-workspace")
	for k, v := range h {
		hdr.Set(k, v)
	}
	return cliproxyexecutor.Options{Headers: hdr}
}

func composerExecOpts(format, conv string) cliproxyexecutor.Options {
	opts := optsWithHeaders(map[string]string{"X-Conversation-Id": conv})
	opts.SourceFormat = sdktranslator.FromString(format)
	return opts
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
	// A stable conversation id routes an agentic turn deterministically: the SAME opener under the SAME id is
	// always the same session across turns (identity-finalplan: baseSid stays ID-authoritative).
	a, err := deriveComposerSessionID(authWith("authA", "keyA"), "cursorkey", toolTurn("hello"), convoOpts("conv-1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(a, "sess_") {
		t.Fatalf("session id should be prefixed sess_, got %s", a)
	}
	aSame, _ := deriveComposerSessionID(authWith("authA", "keyA"), "cursorkey", toolTurn("hello"), convoOpts("conv-1"))
	if a != aSame {
		t.Fatalf("same conversation id + same opener must be stable: %s vs %s", a, aSame)
	}
	// identity-finalplan §1.3 (the proven bug fix): a DIFFERENT opener (a divergent context — subagent /
	// branch / parallel fan-out) sharing the SAME id must SPLIT into a distinct session, not collapse onto
	// baseSid the way it did before this fix.
	a2, _ := deriveComposerSessionID(authWith("authA", "keyA"), "cursorkey", toolTurn("a totally different first task"), convoOpts("conv-1"))
	if a == a2 {
		t.Fatalf("a divergent opener under the same conversation id must SPLIT to a distinct session: %s == %s", a, a2)
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
	// metadata.user_id (Claude session) routes an agentic turn deterministically: the SAME opener under the
	// same uuid is the same session across turns.
	o := cliproxyexecutor.Options{OriginalRequest: []byte(`{"metadata":{"user_id":"user_x_account__session_uuid-1"}}`)}
	s1, err := deriveComposerSessionID(auth, "cursorkey", toolTurn("hi"), o)
	if err != nil || s1 == "" {
		t.Fatalf("metadata.user_id should route: %v", err)
	}
	s1b, _ := deriveComposerSessionID(auth, "cursorkey", toolTurn("hi"), o)
	if s1 != s1b {
		t.Fatalf("same Claude session uuid + same opener must be stable: %s vs %s", s1, s1b)
	}
	// identity-finalplan §1.3: a subagent reusing the parent's metadata.user_id but with its OWN first task
	// (a divergent opener) must SPLIT to a distinct fork session rather than collapse onto the parent.
	s1Fork, _ := deriveComposerSessionID(auth, "cursorkey", toolTurn("a different subagent task"), o)
	if s1 == s1Fork {
		t.Fatalf("a divergent opener sharing the parent uuid must split to a distinct session: %s == %s", s1, s1Fork)
	}
	s2, _ := deriveComposerSessionID(auth, "cursorkey", toolTurn("hi"), cliproxyexecutor.Options{OriginalRequest: []byte(`{"metadata":{"user_id":"user_x_account__session_uuid-2"}}`)})
	if s1 == s2 {
		t.Fatalf("different Claude session uuids must differ")
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

func TestComposerSystemDeduplicatesAndBoundsHiddenHooks(t *testing.T) {
	const hook = "Stable project guidance\nUse the declared repository conventions."
	messages := make([]map[string]any, 0, 53)
	messages = append(messages, map[string]any{"role": "system", "content": "base system"})
	for i := 0; i < 50; i++ {
		messages = append(messages, map[string]any{"role": "developer", "content": "\r\n" + hook + "  "})
	}
	messages = append(messages,
		map[string]any{"role": "developer", "content": "distinct update"},
		map[string]any{"role": "user", "content": "ship the selected zip"},
	)
	body, err := json.Marshal(map[string]any{"messages": messages})
	if err != nil {
		t.Fatal(err)
	}
	parsed := gjson.GetBytes(body, "messages").Array()
	system := extractComposerSystem(parsed)
	blocks := extractComposerSystemBlocks(parsed, "")
	if count := strings.Count(system, "Stable project guidance"); count != 1 {
		t.Fatalf("duplicate developer hooks must collapse to one copy, got %d in %q", count, system)
	}
	if !strings.Contains(system, "base system") || !strings.Contains(system, "distinct update") {
		t.Fatalf("unique system/developer blocks must retain input order, got %q", system)
	}
	if len(blocks) != 3 || composerSystemBlocksText(blocks) != system {
		t.Fatalf("structured blocks must exactly describe aggregate context: %#v / %q", blocks, system)
	}
	for _, block := range blocks {
		if !strings.HasPrefix(block.ID, "csb1_") || block.Text == "" {
			t.Fatalf("system block needs stable identity and non-empty text: %#v", block)
		}
	}

	base := composerInput([]byte(`{"messages":[{"role":"developer","content":"same hook"},{"role":"user","content":"do it"}]}`))
	duplicates := composerInput([]byte(`{"messages":[{"role":"developer","content":"same hook"},{"role":"developer","content":" same hook "},{"role":"user","content":"do it"}]}`))
	if base["clientMessageId"] != duplicates["clientMessageId"] {
		t.Fatalf("semantically duplicate hidden hooks must not change retry identity: %v vs %v", base["clientMessageId"], duplicates["clientMessageId"])
	}
	baseBlocks, ok := base["systemBlocks"].([]composerSystemBlock)
	if !ok || len(baseBlocks) != 1 {
		t.Fatalf("fresh input must carry one structured system block, got %#v", base["systemBlocks"])
	}
	duplicateBlocks, ok := duplicates["systemBlocks"].([]composerSystemBlock)
	if !ok || !reflect.DeepEqual(baseBlocks, duplicateBlocks) {
		t.Fatalf("duplicate hook replay changed delivered block identity: %#v vs %#v", baseBlocks, duplicates["systemBlocks"])
	}

	appended := composerInput([]byte(`{"messages":[{"role":"developer","content":"same hook"},{"role":"developer","content":"new suffix"},{"role":"user","content":"do it"}]}`))
	appendedBlocks, ok := appended["systemBlocks"].([]composerSystemBlock)
	if !ok || len(appendedBlocks) != 2 || appendedBlocks[0].ID != baseBlocks[0].ID {
		t.Fatalf("ordinary context growth must preserve an exact block-ID prefix: %#v", appended["systemBlocks"])
	}
	replaced := composerInput([]byte(`{"messages":[{"role":"developer","content":"replacement"},{"role":"user","content":"do it"}]}`))
	replacedBlocks, ok := replaced["systemBlocks"].([]composerSystemBlock)
	if !ok || len(replacedBlocks) != 1 || replacedBlocks[0].ID == baseBlocks[0].ID {
		t.Fatalf("replacement context must not masquerade as the original block: %#v", replaced["systemBlocks"])
	}

	saved := composerSystemMaxBytes
	composerSystemMaxBytes = 4096
	defer func() { composerSystemMaxBytes = saved }()
	huge := strings.Repeat("界", 4000)
	boundedBody, _ := json.Marshal(map[string]any{"messages": []map[string]any{
		{"role": "system", "content": "HEAD " + huge},
		{"role": "developer", "content": huge + " TAIL"},
	}})
	bounded := extractComposerSystem(gjson.GetBytes(boundedBody, "messages").Array())
	boundedBlocks := extractComposerSystemBlocks(gjson.GetBytes(boundedBody, "messages").Array(), "")
	if len(bounded) > composerSystemMaxBytes {
		t.Fatalf("system/developer aggregate exceeded strict cap: %d > %d", len(bounded), composerSystemMaxBytes)
	}
	if !utf8.ValidString(bounded) || !strings.Contains(bounded, "content omitted inside this system/developer instruction block") {
		t.Fatalf("bounded instructions must remain valid UTF-8 and mark omission, len=%d", len(bounded))
	}
	if composerSystemBlocksText(boundedBlocks) != bounded {
		t.Fatalf("bounded structured blocks diverged from aggregate text: %#v", boundedBlocks)
	}
	for _, block := range boundedBlocks {
		if !utf8.ValidString(block.Text) {
			t.Fatalf("bounded block split UTF-8: %#v", block)
		}
	}

	fullNoSystem := composerInput([]byte(`{"messages":[{"role":"user","content":"fresh stateless turn"}]}`))
	if blocks, ok := fullNoSystem["systemBlocks"].([]composerSystemBlock); !ok || len(blocks) != 0 {
		t.Fatalf("a full stateless snapshot must signal authoritative empty system context: %#v", fullNoSystem)
	}
	thinChained := composerInput([]byte(`{"previous_response_id":"resp_existing","messages":[{"role":"user","content":"thin chained turn"}]}`))
	if _, exists := thinChained["systemBlocks"]; exists {
		t.Fatalf("a thin server-chained turn must inherit omitted system context, got %#v", thinChained)
	}
}

func TestComposerFreshUserTurnHasRetryStableMessageID(t *testing.T) {
	request := []byte(`{"messages":[{"role":"system","content":"S"},{"role":"user","content":"do one thing"}]}`)
	first := composerInput(request)
	retry := composerInput(request)
	id, _ := first["clientMessageId"].(string)
	if !strings.HasPrefix(id, "ccm2_") || retry["clientMessageId"] != id {
		t.Fatalf("fresh request retries need one stable message id: %v vs %v", first["clientMessageId"], retry["clientMessageId"])
	}
	changed := composerInput([]byte(`{"messages":[{"role":"system","content":"S"},{"role":"user","content":"do a different thing"}]}`))
	if changed["clientMessageId"] == id {
		t.Fatalf("distinct active user intent reused message id %q", id)
	}
	changedSystem := composerInput([]byte(`{"messages":[{"role":"system","content":"different system"},{"role":"user","content":"do one thing"}]}`))
	if changedSystem["clientMessageId"] == id {
		t.Fatalf("a system-contract change must not reuse the prior turn id %q", id)
	}
	repeatedLater := composerInput([]byte(`{"messages":[
		{"role":"user","content":"do one thing"},
		{"role":"assistant","content":"done"},
		{"role":"user","content":"do one thing"}
	]}`))
	if repeatedLater["clientMessageId"] == id {
		t.Fatalf("a later intentional repetition must not reuse the first turn id %q", id)
	}
}

func TestComposerInvocationIDExtraction(t *testing.T) {
	jsonUserID := `{"session_id":"session-1","turn_id":"inv1_turn-0001"}`
	original, _ := json.Marshal(map[string]any{"metadata": map[string]any{"user_id": jsonUserID}})
	if got := composerInvocationID(nil, cliproxyexecutor.Options{OriginalRequest: original}); got != "inv1_turn-0001" {
		t.Fatalf("JSON metadata invocation id = %q", got)
	}

	opts := optsWithHeaders(map[string]string{
		"Idempotency-Key": "inv1_standard-0002",
	})
	if got := composerInvocationID(nil, opts); got != "inv1_standard-0002" {
		t.Fatalf("standard header invocation id = %q", got)
	}

	clientTurn := optsWithHeaders(map[string]string{"X-Client-Turn-ID": "inv1_client-0003"})
	if got := composerInvocationID(nil, clientTurn); got != "inv1_client-0003" {
		t.Fatalf("client turn header invocation id = %q", got)
	}

	cliproxyHeader := optsWithHeaders(map[string]string{"X-CLIProxy-Invocation-ID": "inv1_cliproxy-0004"})
	if got := composerInvocationID(nil, cliproxyHeader); got != "inv1_cliproxy-0004" {
		t.Fatalf("cliproxy invocation header = %q", got)
	}

	fromMeta := cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.InvocationIDMetadataKey: "inv1_meta-0005"}}
	if got := composerInvocationID(nil, fromMeta); got != "inv1_meta-0005" {
		t.Fatalf("metadata invocation id = %q", got)
	}

	invalid, _ := json.Marshal(map[string]any{"metadata": map[string]any{"turn_id": "contains spaces"}})
	if got := composerInvocationID(invalid, cliproxyexecutor.Options{OriginalRequest: invalid}); got != "" {
		t.Fatalf("invalid invocation id must be ignored, got %q", got)
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
	// tool_choice=none path: caller passes advertise=nil => an explicit empty
	// inventory clears any tools cached by a reused bridge session.
	none := composerTurnBody("s1", "composer-2.5", map[string]any{"type": "user", "text": "x"}, nil, "none", nil, nil, 0)
	if gjson.GetBytes(none, "tools").Exists() {
		t.Fatalf("tool inventory must not be serialized twice: %s", none)
	}
	if tools := gjson.Parse(gjson.GetBytes(none, "toolInventoryJSON").String()); !tools.IsArray() || len(tools.Array()) != 0 {
		t.Fatalf("tool_choice=none must send an explicit empty inventory: %s", none)
	}
	if gjson.GetBytes(none, "toolChoice").String() != "none" {
		t.Fatalf("toolChoice not forwarded: %s", none)
	}
	// With advertised tools + clientEnv + enforced constraints.
	adv := []map[string]any{{"name": "Read", "toolName": "Read"}}
	full := composerTurnBody("s1", "composer-2.5", map[string]any{"type": "user", "text": "x"}, adv, "auto", map[string]any{"shell": "zsh"},
		map[string]any{"responseFormat": map[string]any{"type": "json_object"}, "stop": []string{"STOP"}, "maxTokens": 256}, 17)
	if gjson.Parse(gjson.GetBytes(full, "toolInventoryJSON").String()).Get("0.name").String() != "Read" {
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
	if gjson.GetBytes(full, "clientLeaseToken").String() != "17" {
		t.Fatalf("fresh-turn lease token must cross the JS boundary as lossless decimal text: %s", full)
	}
}

// Comment 2: a data-URI image sent through cursor_composer.go must reach the sidecar in the SDK image
// shape {data, mimeType} — the MIME type must survive the Go->bridge translation.
func TestComposerImageCarriesMimeType(t *testing.T) {
	oai := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,SkZJRg=="}}]}]}`)
	body := composerTurnBody("s1", "composer-2.5", composerInput(oai), nil, "", nil, composerConstraints(oai), 0)
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

// A mixed tool_results+text turn is two independent facts: an immutable tool
// receipt and a separately identified user message. The bridge journals and
// delivers the latter without corrupting the former's idempotency hash.
func TestComposerMixedTurnSeparatesTrailingText(t *testing.T) {
	mixed := composerInput([]byte(`{"messages":[{"role":"assistant","content":"","tool_calls":[{"id":"tc_1","function":{"name":"Read"}}]},{"role":"tool","tool_call_id":"tc_1","content":"R"},{"role":"user","content":"also do X"}]}`))
	if mixed["type"] != "tool_results" {
		t.Fatalf("mixed turn must classify as tool_results, got %v", mixed["type"])
	}
	results, _ := mixed["results"].([]map[string]any)
	if len(results) != 1 {
		t.Fatalf("expected 1 tool result, got %d", len(results))
	}
	if content, _ := results[len(results)-1]["content"].(string); content != "R" {
		t.Fatalf("tool result receipt must remain immutable, got %q", content)
	}
	if mixed["userText"] != "also do X" || mixed["interruptRequested"] != true {
		t.Fatalf("trailing user input must be first-class and interrupt-capable, got %#v", mixed)
	}
	if id, _ := mixed["clientMessageId"].(string); !strings.HasPrefix(id, "ccm2_") {
		t.Fatalf("mixed input needs a stable client message id, got %q", id)
	}
	// A pure tool_results turn keeps its result immutable and still receives a
	// stable request id so a lost final response can be replayed without a
	// second SDK send.
	pure := composerInput([]byte(`{"messages":[{"role":"assistant","content":"","tool_calls":[{"id":"tc_1","function":{"name":"Read"}}]},{"role":"tool","tool_call_id":"tc_1","content":"R"}]}`))
	pres, _ := pure["results"].([]map[string]any)
	if len(pres) != 1 {
		t.Fatalf("pure: expected 1 result, got %d", len(pres))
	}
	if c, _ := pres[0]["content"].(string); c != "R" {
		t.Fatalf("a pure tool_results turn must NOT be modified, got content %q", c)
	}
	if id, _ := pure["clientMessageId"].(string); !strings.HasPrefix(id, "ccm2_") {
		t.Fatalf("pure result continuation needs a stable response-replay id, got %q", id)
	}
}

func TestComposerClientMessageIDIsRetryStableAndTurnDistinct(t *testing.T) {
	request := []byte(`{"messages":[
		{"role":"assistant","tool_calls":[{"id":"tc_1","function":{"name":"Read"}}]},
		{"role":"tool","tool_call_id":"tc_1","content":"R"},
		{"role":"user","content":"same text"}
	]}`)
	first := composerInput(request)
	retry := composerInput(request)
	if first["clientMessageId"] == "" || first["clientMessageId"] != retry["clientMessageId"] {
		t.Fatalf("exact request retries need one stable message id: %v vs %v", first["clientMessageId"], retry["clientMessageId"])
	}
	distinct := composerInput([]byte(`{"messages":[
		{"role":"user","content":"earlier turn"},
		{"role":"assistant","tool_calls":[{"id":"tc_2","function":{"name":"Read"}}]},
		{"role":"tool","tool_call_id":"tc_2","content":"R"},
		{"role":"user","content":"same text"}
	]}`))
	if first["clientMessageId"] == distinct["clientMessageId"] {
		t.Fatalf("same text in distinct turns must have distinct ids: %v", first["clientMessageId"])
	}

	reencoded := composerInput([]byte(`{
		"messages" : [
			{"tool_calls":[{"function":{"name":"Read"},"id":"tc_1"}],"role":"assistant"},
			{"content":"R","tool_call_id":"tc_1","role":"tool"},
			{"content":"same \u0074ext","role":"user"}
		]
	}`))
	if first["clientMessageId"] != reencoded["clientMessageId"] {
		t.Fatalf("semantic retry changed id after harmless JSON reserialization: %v vs %v", first["clientMessageId"], reencoded["clientMessageId"])
	}
	singleID := composerClientMessageID([]map[string]any{{"toolCallId": "tc_1"}}, "same text", nil)
	duplicateID := composerClientMessageID([]map[string]any{
		{"toolCallId": "tc_1"},
		{"toolCallId": "tc_1"},
	}, "same text", nil)
	if singleID != duplicateID {
		t.Fatalf("idempotent duplicate result ids changed client message identity: %q vs %q", singleID, duplicateID)
	}
}

func TestPrepareComposerInboundStockClientExactRetryReattachesLease(t *testing.T) {
	e := NewCursorExecutor(&config.Config{})
	auth := authWith("stock-lease-auth", "stock-lease-key")
	opts := composerExecOpts("openai", "stock-lease-conversation")
	req := cliproxyexecutor.Request{Model: "composer-2.5", Payload: toolTurn("same stock-client request")}

	first, err := e.prepareComposerInbound(auth, "stock-lease-key", req, opts, true)
	if err != nil {
		t.Fatalf("first prepare: %v", err)
	}
	defer composerInflight.release(first.tenant, first.sessionID, first.leaseOwner)
	second, err := e.prepareComposerInbound(auth, "stock-lease-key", req, opts, true)
	if err != nil {
		t.Fatalf("retry prepare: %v", err)
	}
	if first.sessionID != second.sessionID || first.leaseOwner != second.leaseOwner {
		t.Fatalf("stock retry forked instead of reattaching: first=%s/%d second=%s/%d",
			first.sessionID, first.leaseOwner, second.sessionID, second.leaseOwner)
	}
	clientMessageID, _ := first.input["clientMessageId"].(string)
	if !strings.HasPrefix(clientMessageID, "ccm2_") {
		t.Fatalf("stock compatibility identity = %q, want ccm2_", clientMessageID)
	}
	if _, explicit := first.input["invocationId"]; explicit {
		t.Fatalf("stock request unexpectedly gained a client-supplied invocationId: %#v", first.input)
	}
}

func TestExecuteComposerEstablishedStreamReconnectsStockClientWithoutDuplicateFrames(t *testing.T) {
	t.Setenv("CURSOR_COMPOSER_STREAM_RECONNECT_MAX_ATTEMPTS", "1")
	t.Setenv("CURSOR_COMPOSER_ADMISSION_MAX_ACTIVE_PER_KEY", "1")
	t.Setenv("CURSOR_COMPOSER_ADMISSION_MIN_GAP_MS", "0")
	var mu sync.Mutex
	calls := 0
	var bodies [][]byte
	var authHeaders []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls++
		call := calls
		bodies = append(bodies, append([]byte(nil), raw...))
		authHeaders = append(authHeaders, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		write := func(payload string) {
			_, _ = io.WriteString(w, "data: "+payload+"\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		}
		write(`{"type":"ping"}`)
		write(`{"type":"reasoning","delta":"think"}`)
		write(`{"type":"text","delta":"hel"}`)
		if call == 1 {
			return // accepted request, partial frames, then bridge EOF without terminal
		}
		write(`{"type":"text","delta":"lo"}`)
		write(`{"type":"turn_end","stop_reason":"end_turn"}`)
		write("[DONE]")
	}))
	defer srv.Close()

	e := NewCursorExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{ID: "stock-stream-reattach", Attributes: map[string]string{
		"api_key":                          "same-key",
		"composer_client_tools_bridge_url": srv.URL,
	}}
	req := cliproxyexecutor.Request{Model: "composer-2.5", Payload: toolTurn("recover this stock request")}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	stream, err := e.executeComposerStream(ctx, auth, "same-key", req, composerExecOpts("openai", "stock-stream-reattach"))
	if err != nil {
		t.Fatalf("stream setup: %v", err)
	}
	var wire strings.Builder
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			t.Fatalf("post-connect recovery surfaced an error: %v", chunk.Err)
		}
		wire.Write(chunk.Payload)
	}
	got := wire.String()
	if !strings.Contains(got, ": keepalive") {
		t.Fatalf("keepalive was not forwarded during atomic recovery: %q", got)
	}
	if strings.Count(got, `"reasoning_content":"think"`) != 1 ||
		strings.Count(got, `"content":"hel"`) != 1 || strings.Count(got, `"content":"lo"`) != 1 {
		t.Fatalf("replayed model frames were duplicated or lost: %s", got)
	}

	mu.Lock()
	defer mu.Unlock()
	if calls != 2 || !bytes.Equal(bodies[0], bodies[1]) {
		t.Fatalf("same-invocation reattach calls=%d bodiesEqual=%v", calls, len(bodies) == 2 && bytes.Equal(bodies[0], bodies[1]))
	}
	for i, header := range authHeaders {
		if header != "Bearer same-key" {
			t.Fatalf("attempt %d rotated credential: %q", i+1, header)
		}
	}
	if !strings.HasPrefix(gjson.GetBytes(bodies[0], "input.clientMessageId").String(), "ccm2_") ||
		gjson.GetBytes(bodies[0], "input.invocationId").Exists() {
		t.Fatalf("test did not exercise stock ccm2 fallback: %s", bodies[0])
	}
}

func TestExecuteComposerEstablishedResponseReconnectsWithoutDuplicateContent(t *testing.T) {
	t.Setenv("CURSOR_COMPOSER_STREAM_RECONNECT_MAX_ATTEMPTS", "1")
	var mu sync.Mutex
	calls := 0
	var bodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls++
		call := calls
		bodies = append(bodies, append([]byte(nil), raw...))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"type\":\"reasoning\",\"delta\":\"think\"}\n\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"text\",\"delta\":\"hel\"}\n\n")
		if call == 1 {
			return
		}
		_, _ = io.WriteString(w, "data: {\"type\":\"text\",\"delta\":\"lo\"}\n\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"turn_end\",\"stop_reason\":\"end_turn\"}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	e := NewCursorExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{ID: "stock-response-reattach", Attributes: map[string]string{
		"api_key":                          "same-key",
		"composer_client_tools_bridge_url": srv.URL,
	}}
	req := cliproxyexecutor.Request{Model: "composer-2.5", Payload: toolTurn("recover this non-stream request")}
	resp, err := e.executeComposer(context.Background(), auth, "same-key", req, composerExecOpts("openai", "stock-response-reattach"))
	if err != nil {
		t.Fatalf("non-stream post-connect recovery: %v", err)
	}
	if got := gjson.GetBytes(resp.Payload, "choices.0.message.content").String(); got != "hello" {
		t.Fatalf("recovered content = %q, want hello: %s", got, resp.Payload)
	}
	if got := gjson.GetBytes(resp.Payload, "choices.0.message.reasoning_content").String(); got != "think" {
		t.Fatalf("recovered reasoning = %q, want think: %s", got, resp.Payload)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 2 || !bytes.Equal(bodies[0], bodies[1]) {
		t.Fatalf("non-stream reattach calls=%d bodiesEqual=%v", calls, len(bodies) == 2 && bytes.Equal(bodies[0], bodies[1]))
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
		composerExecOpts("openai", "err1"))
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

func TestComposerRetryableCancellationTerminalMapsToTyped503(t *testing.T) {
	retryable := gjson.Parse(`{"type":"turn_end","stop_reason":"error","retryable":true,"error":"bridge shutdown"}`)
	err := composerBridgeTurnFailure("resp_retryable_cancel", retryable)
	var statusErr cliproxyexecutor.StatusError
	if !errors.As(err, &statusErr) || statusErr.StatusCode() != http.StatusServiceUnavailable {
		t.Fatalf("retryable cancellation terminal must map to typed 503, got %T %v", err, err)
	}
	if strings.Contains(err.Error(), "bridge shutdown") {
		t.Fatalf("client-facing retryable error must not expose internal cancellation detail: %v", err)
	}

	terminal := gjson.Parse(`{"type":"turn_end","stop_reason":"error","retryable":false,"error":"contract failed"}`)
	err = composerBridgeTurnFailure("resp_terminal_cancel", terminal)
	if errors.As(err, &statusErr) {
		t.Fatalf("non-retryable bridge failure must retain ordinary terminal semantics, got typed status: %v", err)
	}
	if !strings.Contains(err.Error(), "contract failed") {
		t.Fatalf("ordinary bridge terminal lost its error: %v", err)
	}
}

func TestComposerAmbiguousTrailingUserSegmentsFailClosed(t *testing.T) {
	messages := gjson.Parse(`[
		{"role":"user","content":"transfer the newest bundle"},
		{"role":"user","content":"standing repository discovery instructions"}
	]`).Array()
	if !composerAmbiguousTrailingUserSegments(messages) {
		t.Fatal("adjacent non-empty user-role suffix must be treated as provenance-ambiguous")
	}

	unambiguous := gjson.Parse(`[
		{"role":"user","content":"earlier turn"},
		{"role":"assistant","content":"reply"},
		{"role":"user","content":"current turn"}
	]`).Array()
	if composerAmbiguousTrailingUserSegments(unambiguous) {
		t.Fatal("ordinary alternating conversation must remain accepted")
	}
}

func TestAnthropicMessagesAdjacentUserSuffixCombinesBeforeComposer(t *testing.T) {
	tests := []struct {
		name     string
		messages string
		wantText []string
	}{
		{
			name: "two user records",
			messages: `[
				{"role":"user","content":"goal context"},
				{"role":"user","content":"visible request"}
			]`,
			wantText: []string{"goal context", "visible request"},
		},
		{
			name: "four user continuation records",
			messages: `[
				{"role":"user","content":"goal context"},
				{"role":"user","content":"visible request"},
				{"role":"user","content":"refreshed goal context"},
				{"role":"user","content":"continuation"}
			]`,
			wantText: []string{"goal context", "visible request", "refreshed goal context", "continuation"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := []byte(`{"model":"composer-2.5","messages":` + tt.messages + `}`)
			opts := composerExecOpts("claude", "anthropic-combined-wire-"+strings.ReplaceAll(tt.name, " ", "-"))
			opts.OriginalRequest = append([]byte(nil), payload...)
			opts.Metadata = map[string]any{cliproxyexecutor.RequestPathMetadataKey: "/v1/messages"}
			auth := &cliproxyauth.Auth{ID: "anthropic-combined", Attributes: map[string]string{"api_key": "cursor-key"}}

			turn, err := NewCursorExecutor(&config.Config{}).prepareComposerInbound(
				auth,
				"cursor-key",
				cliproxyexecutor.Request{Model: "composer-2.5", Payload: payload},
				opts,
				false,
			)
			if err != nil {
				t.Fatalf("prepareComposerInbound() error = %v", err)
			}

			var users []gjson.Result
			for _, message := range gjson.GetBytes(turn.oai, "messages").Array() {
				if message.Get("role").String() == "user" {
					users = append(users, message)
				}
			}
			if len(users) != 1 {
				t.Fatalf("translated user messages = %s, want one combined turn", gjson.GetBytes(turn.oai, "messages").Raw)
			}

			var gotText []string
			for _, part := range users[0].Get("content").Array() {
				if part.Get("type").String() == "text" {
					gotText = append(gotText, part.Get("text").String())
				}
			}
			if !reflect.DeepEqual(gotText, tt.wantText) {
				t.Fatalf("combined text blocks = %#v, want %#v", gotText, tt.wantText)
			}
		})
	}
}

func TestManagerAdjacentUserSuffixDoesNotRotateOrPoisonCredentials(t *testing.T) {
	var bridgeCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bridgeCalls.Add(1)
		http.Error(w, "bridge must not be called", http.StatusInternalServerError)
	}))
	defer srv.Close()

	payload := []byte(`{
		"model":"composer-2.5",
		"messages":[
			{"role":"user","content":"perform the requested operation"},
			{"role":"user","content":"standing injected context"}
		]
	}`)
	opts := composerExecOpts("openai", "ambiguous-manager-wire")
	opts.OriginalRequest = append([]byte(nil), payload...)
	opts.Metadata = map[string]any{cliproxyexecutor.RequestPathMetadataKey: "/v1/messages"}

	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "nonstream", true: "stream"}[stream], func(t *testing.T) {
			manager := cliproxyauth.NewManager(nil, &cliproxyauth.RoundRobinSelector{}, nil)
			manager.RegisterExecutor(NewCursorExecutor(&config.Config{}))
			for _, id := range []string{"ambiguous-a", "ambiguous-b"} {
				_, err := manager.Register(context.Background(), &cliproxyauth.Auth{
					ID:       id,
					Provider: "cursor",
					Attributes: map[string]string{
						"api_key":                          "cursor-key",
						"composer_client_tools_bridge_url": srv.URL,
					},
				})
				if err != nil {
					t.Fatalf("register %s: %v", id, err)
				}
				registry.GetGlobalRegistry().RegisterClient(id, "cursor", []*registry.ModelInfo{{ID: "composer-2.5"}})
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
			}

			req := cliproxyexecutor.Request{Model: "composer-2.5", Payload: payload}
			var err error
			if stream {
				_, err = manager.ExecuteStream(context.Background(), []string{"cursor"}, req, opts)
			} else {
				_, err = manager.Execute(context.Background(), []string{"cursor"}, req, opts)
			}
			if err == nil {
				t.Fatal("ambiguous adjacent user-role suffix must fail closed")
			}
			var statusErr cliproxyexecutor.StatusError
			if !errors.As(err, &statusErr) || statusErr.StatusCode() != http.StatusUnprocessableEntity {
				t.Fatalf("manager error = %T %v, want typed 422 clarification", err, err)
			}
			if !strings.Contains(err.Error(), "provenance_ambiguous") {
				t.Fatalf("manager error missing provenance_ambiguous marker: %v", err)
			}
			if got := bridgeCalls.Load(); got != 0 {
				t.Fatalf("bridge/model/tool execution calls = %d, want zero", got)
			}
			for _, id := range []string{"ambiguous-a", "ambiguous-b"} {
				auth, ok := manager.GetByID(id)
				if !ok || auth.Unavailable || auth.LastError != nil {
					t.Fatalf("credential %s was poisoned by request provenance rejection: %#v", id, auth)
				}
			}
		})
	}
}

func TestCursorDirectAdjacentUserSuffixFailsBeforeCredentialExchange(t *testing.T) {
	var outboundCalls atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		outboundCalls.Add(1)
		http.Error(w, "credential exchange must not be called", http.StatusInternalServerError)
	}))
	defer proxy.Close()

	payload := []byte(`{
		"model":"composer-2.5",
		"messages":[
			{"role":"user","content":"perform the requested operation"},
			{"role":"user","content":"standing injected context"}
		]
	}`)
	auth := &cliproxyauth.Auth{ID: "direct-ambiguous", ProxyURL: proxy.URL}
	opts := composerExecOpts("openai", "direct-ambiguous-wire")
	executor := NewCursorExecutor(&config.Config{})
	for _, stream := range []bool{false, true} {
		req := cliproxyexecutor.Request{Model: "composer-2.5", Payload: append([]byte(nil), payload...)}
		_, err := executor.prepareCursorDirect(context.Background(), auth, "cursor-key", &req, opts, stream)
		if err == nil {
			t.Fatalf("stream=%t: adjacent user-role suffix must fail closed", stream)
		}
		var statusErr cliproxyexecutor.StatusError
		if !errors.As(err, &statusErr) || statusErr.StatusCode() != http.StatusUnprocessableEntity {
			t.Fatalf("stream=%t: error = %T %v, want typed 422 clarification", stream, err, err)
		}
		if !strings.Contains(err.Error(), "provenance_ambiguous") {
			t.Fatalf("stream=%t: error missing provenance_ambiguous marker: %v", stream, err)
		}
	}
	if got := outboundCalls.Load(); got != 0 {
		t.Fatalf("credential/upstream calls = %d, want zero", got)
	}
}

func TestExecuteComposerRetryableTerminalReattachesSameInvocation(t *testing.T) {
	t.Setenv("CURSOR_COMPOSER_STREAM_RECONNECT_MAX_ATTEMPTS", "1")
	var mu sync.Mutex
	calls := 0
	var bodies [][]byte
	var authHeaders []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls++
		call := calls
		bodies = append(bodies, append([]byte(nil), raw...))
		authHeaders = append(authHeaders, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if call%2 == 1 {
			_, _ = w.Write([]byte("data: {\"type\":\"turn_end\",\"stop_reason\":\"error\",\"receipt\":\"continuation_conflict_contained\",\"retryable\":true,\"retryMode\":\"identical\",\"errorCode\":\"journal_revision_conflict\",\"error\":\"retry the identical continuation\"}\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			return
		}
		_, _ = w.Write([]byte("data: {\"type\":\"text\",\"delta\":\"recovered exactly once\"}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"turn_end\",\"stop_reason\":\"end_turn\"}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	e := NewCursorExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{ID: "contained", Attributes: map[string]string{
		"api_key":                          "k",
		"composer_client_tools_bridge_url": srv.URL,
	}}
	req := cliproxyexecutor.Request{
		Model:   "composer-2.5",
		Payload: []byte(`{"model":"composer-2.5","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"Read"}}]}`),
	}

	resp, err := e.executeComposer(context.Background(), auth, "k", req, composerExecOpts("openai", "contained-nonstream"))
	if err != nil {
		t.Fatalf("non-stream retryable terminal should recover internally: %v", err)
	}
	if got := gjson.GetBytes(resp.Payload, "choices.0.message.content").String(); got != "recovered exactly once" {
		t.Fatalf("non-stream recovered content = %q: %s", got, resp.Payload)
	}

	stream, err := e.executeComposerStream(context.Background(), auth, "k", req, composerExecOpts("openai", "contained-stream"))
	if err != nil {
		t.Fatalf("stream retryable terminal setup: %v", err)
	}
	var wire strings.Builder
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream retryable terminal should recover internally: %v", chunk.Err)
		}
		wire.Write(chunk.Payload)
	}
	if got := wire.String(); !strings.Contains(got, "recovered exactly once") || strings.Contains(got, "retry the identical continuation") {
		t.Fatalf("stream recovery leaked retry terminal or lost content: %s", got)
	}

	mu.Lock()
	defer mu.Unlock()
	if calls != 4 {
		t.Fatalf("requests = %d, want one initial + one same-invocation retry per mode", calls)
	}
	for _, pair := range [][2]int{{0, 1}, {2, 3}} {
		if !bytes.Equal(bodies[pair[0]], bodies[pair[1]]) {
			t.Fatalf("retry body changed between attempts %d and %d", pair[0]+1, pair[1]+1)
		}
	}
	for i, header := range authHeaders {
		if header != "Bearer k" {
			t.Fatalf("attempt %d changed credential: %q", i+1, header)
		}
	}
}

func TestComposerBridgeTerminalReplaySafeIsExplicitAndSplitAware(t *testing.T) {
	for name, raw := range map[string]string{
		"explicit identical":       `{"stop_reason":"error","receipt":"future_receipt","retryable":true,"retryMode":"identical"}`,
		"explicit split":           `{"stop_reason":"error","receipt":"continuation_conflict_contained","retryable":true,"retryMode":"split"}`,
		"same invocation conflict": `{"stop_reason":"error","receipt":"continuation_conflict_contained","retryable":true,"errorCode":"journal_revision_conflict"}`,
		"definitive conflict":      `{"stop_reason":"error","receipt":"continuation_conflict_contained","retryable":true,"errorCode":"result_conflict"}`,
		"completion not durable":   `{"stop_reason":"error","receipt":"completion_not_durable","retryable":true}`,
		"unknown retryable":        `{"stop_reason":"error","receipt":"some_future_error","retryable":true}`,
		"not retryable":            `{"stop_reason":"error","receipt":"continuation_conflict_contained","retryable":false}`,
		"not error":                `{"stop_reason":"stop","receipt":"continuation_conflict_contained","retryable":true}`,
		"must split conversations": `{"stop_reason":"error","receipt":"multiple_live_tool_rounds_deferred","retryable":true}`,
	} {
		t.Run(name, func(t *testing.T) {
			got := composerBridgeTerminalReplaySafe(gjson.Parse(raw))
			want := name == "explicit identical" || name == "same invocation conflict" || name == "completion not durable"
			if got != want {
				t.Fatalf("composerBridgeTerminalReplaySafe() = %v, want %v", got, want)
			}
		})
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

func TestComposerAdvertiseCarriesAliasesToBridge(t *testing.T) {
	oai := []byte(`{"tools":[
		{"type":"function","function":{"name":"RunCommand","parameters":{"type":"object","properties":{"command":{"type":"string"}}}}},
		{"type":"function","function":{"name":"Read","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}
	]}`)
	defs := composerToolDefs(oai)
	prep := prepareComposerAdvertise(oai, defs, map[string]string{"shell": "RunCommand", "terminal": "RunCommand"})
	if len(prep.advertise) != 2 {
		t.Fatalf("expected two advertised tools, got %#v", prep.advertise)
	}
	aliases, _ := prep.advertise[0]["aliases"].([]string)
	if !reflect.DeepEqual(aliases, []string{"shell", "terminal"}) {
		t.Fatalf("explicit aliases must be sorted and attached to RunCommand, got %#v", aliases)
	}
	if _, exists := prep.advertise[1]["aliases"]; exists {
		t.Fatalf("unaliased Read descriptor must not gain aliases: %#v", prep.advertise[1])
	}
}

func TestMapComposerToolCallExactBridgePayloadPassesThrough(t *testing.T) {
	defs := []cursorToolDefinition{{
		Name:       "RunCommand",
		Parameters: `{"type":"object","properties":{"command":{"type":"string"},"description":{"type":"string"}},"required":["command","description"],"additionalProperties":false}`,
	}}
	input := gjson.Parse(`{"command":"pwd"}`)
	name, args := mapComposerToolCall("RunCommand", input, defs, map[string]string{"runcommand": "OtherTool"})
	if name != "RunCommand" || args != input.Raw {
		t.Fatalf("canonical bridge event must pass through unchanged, got name=%q args=%s", name, args)
	}
	if gjson.Get(args, "description").Exists() {
		t.Fatalf("Go must not add a post-journal default to canonical bridge args: %s", args)
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
		return composerExecOpts("openai", "conv-nonstream")
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
	// loss). The accumulated text must survive and the malformed RAW must never be spliced into the body.
	// ACCURATE ACCOUNTING: since the turn produced output and the SDK reports no real usage, the malformed
	// fragment is replaced by a clean non-zero ESTIMATE — so usage is present, well-formed, and NOT the garbage.
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
	// The malformed raw is omitted; a clean, parseable, non-zero estimate stands in (a turn that produced text
	// must not report 0 tokens). total_tokens must be a real integer (proves the garbage was not spliced).
	if tt := gjson.Get(bodyBad, "usage.total_tokens"); !tt.Exists() || tt.Int() <= 0 {
		t.Fatalf("malformed usage must be replaced by a clean non-zero estimate, got: %s", bodyBad)
	}
}

// The bridge's typed {"type":"ping"} keepalive reaches the downstream as an
// SSE comment while model-visible frames remain atomically buffered until a
// durable terminal. Comments are schema-neutral and do not open an assistant
// envelope that a retry would have to duplicate.
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

	// Anthropic client: the comment may precede message_start because compliant
	// SSE parsers ignore it; no model-visible response bytes are committed yet.
	claudeReq := cliproxyexecutor.Request{Model: "composer-2.5", Payload: []byte(`{"model":"composer-2.5","messages":[{"role":"user","content":"hi"}],"tools":[{"name":"Read","input_schema":{"type":"object"}}]}`)}
	srA, err := e.executeComposerStream(context.Background(), auth, "k", claudeReq,
		composerExecOpts("claude", "ping-a"))
	if err != nil {
		t.Fatalf("stream(claude): %v", err)
	}
	outA := collect(srA)
	if !strings.Contains(outA, ": keepalive") {
		t.Fatalf("anthropic keepalive comment not rendered to the client: %q", outA)
	}
	if i, j := strings.Index(outA, `"type":"message"`), strings.Index(outA, ": keepalive"); i < 0 || j < 0 || j > i {
		t.Fatalf("transport comment must precede the atomically committed envelope (msg@%d ping@%d): %q", i, j, outA)
	}
	// The stream MUST close with a terminal (message_stop) — the bridge's [DONE] is consumed by the executor,
	// so it synthesizes one through the translator. Without it the Anthropic SDK hangs waiting to finalize.
	if !strings.Contains(outA, "message_stop") {
		t.Fatalf("anthropic stream missing terminal message_stop (client would hang): %q", outA)
	}

	// OpenAI client: the ping renders as a benign no-op chunk (never a bare comment), and real content survives.
	oaiReq := cliproxyexecutor.Request{Model: "composer-2.5", Payload: []byte(`{"model":"composer-2.5","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"Read","parameters":{"type":"object"}}}]}`)}
	srO, err := e.executeComposerStream(context.Background(), auth, "k", oaiReq,
		composerExecOpts("openai", "ping-o"))
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

func TestExecuteComposerStreamResumeEmitsLiveBeforeTerminal(t *testing.T) {
	releaseTerminal := make(chan struct{})
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
		write(`{"type":"text","delta":"live-hi"}`)
		<-releaseTerminal
		write(`{"type":"turn_end","stop_reason":"end_turn"}`)
		write("[DONE]")
	}))
	defer srv.Close()
	e := NewCursorExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{ID: "authResume", Attributes: map[string]string{"api_key": "k", "composer_client_tools_bridge_url": srv.URL}}

	opts := composerExecOpts("openai", "resume-live")
	opts.Headers.Set(cliproxyexecutor.HeaderCLIProxyCapabilities, cliproxyexecutor.CapabilityStreamResumeV1)
	req := cliproxyexecutor.Request{
		Model:   "composer-2.5",
		Payload: []byte(`{"model":"composer-2.5","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"Read","parameters":{"type":"object"}}}]}`),
	}
	sr, err := e.executeComposerStream(context.Background(), auth, "k", req, opts)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if got := sr.Headers.Get(cliproxyexecutor.HeaderCLIProxyCapabilities); !strings.Contains(got, cliproxyexecutor.CapabilityStreamResumeV1) {
		t.Fatalf("expected stream-resume-v1 advertise, got %q", got)
	}

	sawLive := make(chan struct{})
	done := make(chan string, 1)
	go func() {
		var b strings.Builder
		for chunk := range sr.Chunks {
			if chunk.Err != nil {
				continue
			}
			b.Write(chunk.Payload)
			if strings.Contains(b.String(), "live-hi") {
				select {
				case <-sawLive:
				default:
					close(sawLive)
				}
			}
		}
		done <- b.String()
	}()

	select {
	case <-sawLive:
	case <-time.After(2 * time.Second):
		t.Fatal("stream-resume-v1 must expose journaled/live model bytes before the bridge terminal")
	}
	close(releaseTerminal)
	out := <-done
	if !strings.Contains(out, "live-hi") {
		t.Fatalf("missing live content: %q", out)
	}
}

func TestExecuteComposerStreamLegacyStaysAtomicUntilTerminal(t *testing.T) {
	releaseTerminal := make(chan struct{})
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
		write(`{"type":"text","delta":"atomic-hi"}`)
		<-releaseTerminal
		write(`{"type":"turn_end","stop_reason":"end_turn"}`)
		write("[DONE]")
	}))
	defer srv.Close()
	e := NewCursorExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{ID: "authAtomic", Attributes: map[string]string{"api_key": "k", "composer_client_tools_bridge_url": srv.URL}}

	opts := composerExecOpts("openai", "atomic-legacy")
	req := cliproxyexecutor.Request{
		Model:   "composer-2.5",
		Payload: []byte(`{"model":"composer-2.5","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"Read","parameters":{"type":"object"}}}]}`),
	}
	sr, err := e.executeComposerStream(context.Background(), auth, "k", req, opts)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}

	sawEarly := make(chan struct{})
	done := make(chan string, 1)
	go func() {
		var b strings.Builder
		for chunk := range sr.Chunks {
			if chunk.Err != nil {
				continue
			}
			b.Write(chunk.Payload)
			if strings.Contains(b.String(), "atomic-hi") {
				select {
				case <-sawEarly:
				default:
					close(sawEarly)
				}
			}
		}
		done <- b.String()
	}()

	select {
	case <-sawEarly:
		t.Fatal("legacy clients must not observe model bytes before the durable terminal flush")
	case <-time.After(200 * time.Millisecond):
	}
	close(releaseTerminal)
	out := <-done
	if !strings.Contains(out, "atomic-hi") {
		t.Fatalf("missing atomically flushed content: %q", out)
	}
}

func TestComposerAtomicCommitGlobalBudgetFailsWithoutCredentialAttribution(t *testing.T) {
	savedBudget := composerStreamCommitGlobalBudget
	composerStreamCommitGlobalBudget = &composerAtomicCommitBudget{max: 1}
	defer func() { composerStreamCommitGlobalBudget = savedBudget }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"text\",\"delta\":\"buffered answer\"}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"turn_end\",\"stop_reason\":\"end_turn\"}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()
	auth := &cliproxyauth.Auth{ID: "global-budget", Attributes: map[string]string{
		"api_key":                          "cursor-key",
		"composer_client_tools_bridge_url": srv.URL,
	}}
	executor := NewCursorExecutor(&config.Config{})
	req := cliproxyexecutor.Request{Model: "composer-2.5", Payload: toolTurn("test aggregate admission")}
	stream, err := executor.executeComposerStream(context.Background(), auth, "cursor-key", req, composerExecOpts("openai", "global-budget"))
	if err != nil {
		t.Fatalf("stream setup: %v", err)
	}
	var streamErr error
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			streamErr = chunk.Err
		}
	}
	var capacityErr *composerCommitCapacityError
	if !errors.As(streamErr, &capacityErr) {
		t.Fatalf("stream error = %T %v, want local commit-capacity error", streamErr, streamErr)
	}
	if capacityErr.RetryScope() != cliproxyexecutor.RetryScopeSelectedExecution || capacityErr.AuthAttributable() {
		t.Fatalf("capacity disposition = scope:%v auth:%v", capacityErr.RetryScope(), capacityErr.AuthAttributable())
	}
	if got := composerStreamCommitGlobalBudget.used; got != 0 {
		t.Fatalf("global budget leaked %d bytes after failed stream", got)
	}
}

func TestExecuteComposerNonStreamAggregateIsBounded(t *testing.T) {
	savedMax := composerStreamCommitMaxBytes
	composerStreamCommitMaxBytes = 64
	defer func() { composerStreamCommitMaxBytes = savedMax }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"type\":\"text\",\"delta\":%q}\n\n", strings.Repeat("x", 65))
		_, _ = w.Write([]byte("data: {\"type\":\"turn_end\",\"stop_reason\":\"end_turn\"}\n\n"))
	}))
	defer srv.Close()
	auth := &cliproxyauth.Auth{ID: "nonstream-bound", Attributes: map[string]string{
		"api_key":                          "cursor-key",
		"composer_client_tools_bridge_url": srv.URL,
	}}
	executor := NewCursorExecutor(&config.Config{})
	_, err := executor.executeComposer(context.Background(), auth, "cursor-key", cliproxyexecutor.Request{
		Model:   "composer-2.5",
		Payload: toolTurn("test non-stream aggregate bound"),
	}, composerExecOpts("openai", "nonstream-bound"))
	var protocolErr *composerBridgeProtocolError
	if !errors.As(err, &protocolErr) || !strings.Contains(err.Error(), "protocol violation") {
		t.Fatalf("aggregate overflow error = %T %v, want bounded protocol failure", err, err)
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
	// Mixed: tool_results + trailing text remains a continuation, but the user
	// message is carried independently and never changes a result receipt.
	oai := sdktranslator.TranslateRequest(from, to, "composer-2.5", mk(2, true), false)
	inp := composerInput(oai)
	results, _ := inp["results"].([]map[string]any)
	if inp["type"] != "tool_results" || len(results) != 2 {
		t.Fatalf("mixed tool_result+text must classify as tool_results with 2 results, got type=%v count=%d", inp["type"], len(results))
	}
	if c, _ := results[len(results)-1]["content"].(string); c != "result 1" {
		t.Fatalf("mixed turn must not mutate the last result content, got %q", c)
	}
	if inp["userText"] != "also please continue" || inp["interruptRequested"] != true {
		t.Fatalf("mixed turn must carry user input separately, got %#v", inp)
	}
	if id, _ := inp["clientMessageId"].(string); !strings.HasPrefix(id, "ccm2_") {
		t.Fatalf("mixed turn must carry a stable client message id, got %q", id)
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
	const (
		apiKey    = "cursor-key"
		tenant    = "tenant-a"
		sessionID = "sess_1234567890abcdef"
	)
	a, b := composerResponseID(apiKey, tenant, sessionID), composerResponseID(apiKey, tenant, sessionID)
	if a == b {
		t.Fatalf("response ids must differ per request: %s == %s", a, b)
	}
	if !strings.HasPrefix(a, composerRoutedResponseIDPrefix) {
		t.Fatalf("unexpected response id prefix: %s", a)
	}
	if got := composerSessionFromResponseID(tenant, apiKey, a); got != sessionID {
		t.Fatalf("routed response id decoded to %q, want %q", got, sessionID)
	}
	if got := composerSessionFromResponseID("tenant-b", apiKey, a); got != "" {
		t.Fatalf("cross-tenant response id decoded to %q", got)
	}
	if got := composerSessionFromResponseID(tenant, "different-key", a); got != "" {
		t.Fatalf("cross-key response id decoded to %q", got)
	}
	tampered := a[:len(a)-1] + map[bool]string{true: "A", false: "B"}[a[len(a)-1] != 'A']
	if got := composerSessionFromResponseID(tenant, apiKey, tampered); got != "" {
		t.Fatalf("tampered response id decoded to %q", got)
	}
}

func TestComposerClientToolRoutingTokenPrefixContract(t *testing.T) {
	const expected = "cct1_"
	if composerClientToolRoutingTokenPrefix != expected {
		t.Fatalf("Go routing-token prefix = %q, want %q; update it with ROUTING_TOKEN_VERSION in tool-round.mjs", composerClientToolRoutingTokenPrefix, expected)
	}
	hint := composerContinuationHintFor("", []byte(`{}`))
	if !hint.hasClientToolID("  cct1_route_0_signature  ") {
		t.Fatal("current signed routing-token prefix was not recognized")
	}
	if hint.hasClientToolID("cct2_route_0_signature") {
		t.Fatal("unknown routing-token version was recognized")
	}
}

func TestValidatedComposerClientEnvHeaderlessAndInvalidAreNeutral(t *testing.T) {
	headerless := validatedComposerClientEnv(cliproxyexecutor.Options{})
	if headerless["workspaceUnknown"] != true || headerless["workspacePaths"] != nil || headerless["processWorkingDirectory"] != nil {
		t.Fatalf("headerless Claude-like request must become neutral, got %#v", headerless)
	}
	invalid := optsWithHeaders(map[string]string{
		"X-Cwd":            "relative/proxy/path",
		"X-Workspace-Path": "/app/sidecar-do-not-use",
		"X-Shell":          "zsh",
	})
	got := validatedComposerClientEnv(invalid)
	if got["workspaceUnknown"] != true {
		t.Fatalf("invalid workspace must degrade to neutral instead of 422, got %#v", got)
	}
	if got["workspacePaths"] != nil || got["processWorkingDirectory"] != nil {
		t.Fatalf("invalid headers must not leak a partial proxy/client path: %#v", got)
	}
}

func TestComposerTurnBodyNeutralWorkspaceNeverInventsSentinel(t *testing.T) {
	env := validatedComposerClientEnv(cliproxyexecutor.Options{})
	body := composerTurnBody("sess_test", "cursor-grok-4.5", map[string]any{"type": "user", "text": "inspect this repo"}, nil, "auto", env, nil, 0)
	if !gjson.GetBytes(body, "clientEnv.workspaceUnknown").Bool() {
		t.Fatalf("neutral workspace marker missing: %s", body)
	}
	if gjson.GetBytes(body, "contractVersion").Int() != 2 || !gjson.GetBytes(body, "toolsAuthoritative").Bool() {
		t.Fatalf("client-tool v2 authority contract missing: %s", body)
	}
	if epoch := gjson.GetBytes(body, "toolInventoryEpoch").String(); !strings.HasPrefix(epoch, "cti1_") || len(epoch) != len("cti1_")+32 {
		t.Fatalf("authoritative tool inventory epoch missing or malformed: %q body=%s", epoch, body)
	}
	for _, forbidden := range []string{"/workspace", "/app", `"processWorkingDirectory":"."`} {
		if strings.Contains(string(body), forbidden) {
			t.Fatalf("headerless request invented forbidden workspace %q: %s", forbidden, body)
		}
	}
}

func TestComposerToolInventoryEpochMatchesBridgeCanonicalContract(t *testing.T) {
	tools := []map[string]any{{
		"name":        "T",
		"description": "<&>\u2028",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"0":    map[string]any{"type": "number", "minimum": json.Number("-0")},
				"huge": map[string]any{"type": "integer", "maximum": json.Number("9007199254740993")},
			},
		},
	}}
	const wantJSON = `[{"description":"\u003c\u0026\u003e\u2028","inputSchema":{"properties":{"0":{"minimum":-0,"type":"number"},"huge":{"maximum":9007199254740993,"type":"integer"}},"type":"object"},"name":"T"}]`
	const wantEpoch = "cti1_2bb750689a0806d0a1f5b6e984f6a7d0"
	gotJSON, gotEpoch := composerToolInventorySnapshot(tools)
	if gotJSON != wantJSON {
		t.Fatalf("Go inventory snapshot drifted:\n got %q\nwant %q", gotJSON, wantJSON)
	}
	if gotEpoch != wantEpoch {
		t.Fatalf("Go/bridge inventory fingerprint contract drifted: got %q want %q", gotEpoch, wantEpoch)
	}
	if got := composerToolInventoryEpoch(tools); got != wantEpoch {
		t.Fatalf("Go/bridge inventory fingerprint contract drifted: got %q want %q", got, wantEpoch)
	}
	body := composerTurnBody("s1", "cursor-grok-4.5", map[string]any{"type": "user", "text": "x"}, tools, "auto", nil, nil, 0)
	if got := gjson.GetBytes(body, "toolInventoryJSON").String(); got != wantJSON {
		t.Fatalf("outer turn envelope changed the authoritative snapshot:\n got %q\nwant %q", got, wantJSON)
	}
	if got := gjson.GetBytes(body, "toolInventoryEpoch").String(); got != wantEpoch {
		t.Fatalf("outer turn envelope changed the authoritative epoch: got %q want %q", got, wantEpoch)
	}
}

func TestComposerAdvertisePreservesJSONNumberLexemesUntilAuthoritativeSnapshot(t *testing.T) {
	oai := []byte(`{"tools":[{"type":"function","function":{"name":"T","parameters":{"type":"object","properties":{"n":{"type":"integer","minimum":-0,"maximum":9007199254740993}}}}}]}`)
	tools := composerAdvertise(oai)
	gotJSON, gotEpoch := composerToolInventorySnapshot(tools)
	const wantJSON = `[{"description":"","inputSchema":{"properties":{"n":{"maximum":9007199254740993,"minimum":-0,"type":"integer"}},"type":"object"},"name":"T"}]`
	if gotJSON != wantJSON {
		t.Fatalf("tool-schema JSON numbers changed before the authoritative snapshot:\n got %s\nwant %s", gotJSON, wantJSON)
	}
	sum := sha256.Sum256([]byte(wantJSON))
	wantEpoch := "cti1_" + hex.EncodeToString(sum[:16])
	if gotEpoch != wantEpoch {
		t.Fatalf("snapshot epoch mismatch: got %q want %q", gotEpoch, wantEpoch)
	}
}

func TestComposerToolResultEntryPreservesExplicitStructuredContent(t *testing.T) {
	m := gjson.Parse(`{"role":"tool","tool_call_id":"call_1","content":"plain text","structured_content":{"count":2,"ok":true}}`)
	got := composerToolResultEntry(m)
	if got["content"] != "plain text" {
		t.Fatalf("text content changed: %#v", got["content"])
	}
	structured, ok := got["structuredContent"].(map[string]any)
	if !ok || structured["count"] != json.Number("2") || structured["ok"] != true {
		t.Fatalf("structured content not preserved: %#v", got["structuredContent"])
	}
	textOnly := composerToolResultEntry(gjson.Parse(`{"role":"tool","tool_call_id":"call_2","content":"{\"not\":\"promoted\"}"}`))
	if _, exists := textOnly["structuredContent"]; exists {
		t.Fatal("JSON-looking text must not be promoted to structured content")
	}
}

func TestComposerToolResultEntryPreservesOrderedBlocksAndStrictSemantics(t *testing.T) {
	m := gjson.Parse(`{
		"role":"tool",
		"tool_call_id":"call_blocks",
		"isError":true,
		"content":[
			{"type":"text","text":"first"},
			{"type":"image","source":{"type":"base64","media_type":"IMAGE/PNG","data":"QUJD"}},
			{"type":"text","text":"second"},
			{"type":"image_url","image_url":{"url":"https://example.test/a.png","detail":"high"}}
		],
		"structured_content":null
	}`)
	got := composerToolResultEntry(m)
	if got["content"] != "first\n\nsecond" || got["isError"] != true {
		t.Fatalf("text/error semantics changed: %#v", got)
	}
	if got["structuredContentPresent"] != true {
		t.Fatalf("explicit structured null presence was lost: %#v", got)
	}
	if value, exists := got["structuredContent"]; !exists || value != nil {
		t.Fatalf("explicit structured null was not retained: %#v", got)
	}
	blocks, ok := got["contentBlocks"].([]map[string]any)
	if !ok || len(blocks) != 4 {
		t.Fatalf("ordered content blocks lost: %#v", got["contentBlocks"])
	}
	if blocks[0]["type"] != "text" || blocks[1]["type"] != "image" || blocks[2]["type"] != "text" || blocks[3]["type"] != "image" {
		t.Fatalf("text/image interleaving changed: %#v", blocks)
	}
	images, ok := got["images"].([]map[string]any)
	if !ok || len(images) != 2 || images[1]["detail"] != "high" {
		t.Fatalf("image compatibility envelopes lost fidelity: %#v", got["images"])
	}

	conflict := composerToolResultEntry(gjson.Parse(`{"role":"tool","tool_call_id":"c","content":"x","is_error":true,"isError":false}`))
	if conflict["contractError"] == nil || conflict["isError"] != true {
		t.Fatalf("conflicting error aliases must become a typed contract error: %#v", conflict)
	}
	malformed := composerToolResultEntry(gjson.Parse(`{"role":"tool","tool_call_id":"c","content":[{"type":"image","source":{"type":"base64","data":"***"}}]}`))
	if malformed["contractError"] == nil || malformed["isError"] != true {
		t.Fatalf("malformed image must become a typed contract error: %#v", malformed)
	}
	equalAliases := composerToolResultEntry(gjson.Parse(`{"role":"tool","tool_call_id":"c","content":"x","structured_content":{"a":1,"b":2},"structuredContent":{"b":2,"a":1}}`))
	if equalAliases["contractError"] != nil || equalAliases["structuredContentPresent"] != true {
		t.Fatalf("equivalent structured aliases should deduplicate: %#v", equalAliases)
	}
	conflictingAliases := composerToolResultEntry(gjson.Parse(`{"role":"tool","tool_call_id":"c","content":"x","structured_content":1,"structuredContent":2}`))
	if conflictingAliases["contractError"] == nil || conflictingAliases["isError"] != true {
		t.Fatalf("conflicting structured aliases must fail closed: %#v", conflictingAliases)
	}
	numericAliases := composerToolResultEntry(gjson.Parse(`{"role":"tool","tool_call_id":"c","content":"x","structured_content":1,"structuredContent":1.0}`))
	if numericAliases["contractError"] != nil {
		t.Fatalf("equivalent JSON number spellings must not conflict: %#v", numericAliases)
	}
}

func TestComposerToolResultTruncationPreservesAuthoritativeImageBlocks(t *testing.T) {
	huge := strings.Repeat("x", composerLiveToolResultMaxBytes+1024)
	raw, err := json.Marshal(map[string]any{
		"role":         "tool",
		"tool_call_id": "images-after-truncate",
		"content": []any{
			map[string]any{"type": "text", "text": huge},
			map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "QUJD"}},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.test/a.png", "detail": "high"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := composerToolResultEntry(gjson.ParseBytes(raw))
	blocks, ok := got["contentBlocks"].([]map[string]any)
	if !ok || len(blocks) != 3 || blocks[0]["type"] != "text" || blocks[1]["type"] != "image" || blocks[2]["type"] != "image" {
		t.Fatalf("truncation dropped authoritative images: %#v", got["contentBlocks"])
	}
	if !strings.Contains(blocks[0]["text"].(string), "truncated by proxy") {
		t.Fatalf("oversized text was not visibly truncated: %#v", blocks[0])
	}
	if images, ok := got["images"].([]map[string]any); !ok || len(images) != 2 {
		t.Fatalf("compatibility image projection changed: %#v", got["images"])
	}
}

func TestComposerToolResultUnsafeNumbersBecomeTypedFailuresWithoutHTTPContractErrors(t *testing.T) {
	for _, raw := range []string{
		`{"role":"tool","tool_call_id":"unsafe","content":9007199254740993}`,
		`{"role":"tool","tool_call_id":"unsafe","content":"ok","structured_content":{"n":1e20}}`,
	} {
		got := composerToolResultEntry(gjson.Parse(raw))
		if got["contractError"] != nil || got["isError"] != true {
			t.Fatalf("unrepresentable number must become an accepted typed tool failure, not a 422 contract error: %#v", got)
		}
		structured, _ := got["structuredContent"].(map[string]any)
		if structured["code"] != "client_tool_result_unrepresentable" || structured["executed"] != true || structured["applied"] != false {
			t.Fatalf("missing typed unrepresentable-number receipt: %#v", got)
		}
	}
	oversizedRaw, err := json.Marshal(map[string]any{
		"role":               "tool",
		"tool_call_id":       "oversized-structured",
		"content":            "executed",
		"structured_content": map[string]any{"blob": strings.Repeat("x", composerLiveToolResultMaxBytes+1)},
	})
	if err != nil {
		t.Fatal(err)
	}
	oversized := composerToolResultEntry(gjson.ParseBytes(oversizedRaw))
	if oversized["contractError"] != nil || oversized["isError"] != true {
		t.Fatalf("oversized structured output must become an accepted typed failure rather than a retrying 422: %#v", oversized)
	}

	safe := composerToolResultEntry(gjson.Parse(`{"role":"tool","tool_call_id":"safe","content":{"max":9007199254740991,"negativeZero":-0}}`))
	blocks := safe["contentBlocks"].([]map[string]any)
	value := blocks[0]["value"].(map[string]any)
	if value["max"] != json.Number("9007199254740991") || value["negativeZero"] != json.Number("-0") {
		t.Fatalf("safe integer or negative-zero lexeme changed in Go: %#v", value)
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

// A mixed turn keeps role:user content actionable regardless of tag-like text;
// the proxy never guesses provenance from content conventions.
func TestComposerMixedTurnPreservesTagLikeUserContent(t *testing.T) {
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
	if c, _ := results[len(results)-1]["content"].(string); c != "R" {
		t.Fatalf("real trailing message must not mutate the result receipt: %q", c)
	}

	// User-role content remains user intent regardless of tag-like text. The
	// proxy never infers provenance from content conventions.
	literal := composerInput([]byte(`{"messages":[
		{"role":"assistant","content":"","tool_calls":[{"id":"tc_1","function":{"name":"Read"}}]},
		{"role":"tool","tool_call_id":"tc_1","content":"R"},
		{"role":"user","content":"<context-note>summarize what you just read</context-note>"}
	]}`))
	if literal["userText"] == nil || !strings.Contains(literal["userText"].(string), "summarize what you just read") {
		t.Fatalf("tag-like user content must remain answerable, got %v", literal["userText"])
	}
}

func TestComposerMixedTurnUsesLatestUnresolvedUserSnapshot(t *testing.T) {
	inp := composerInput([]byte(`{"messages":[
		{"role":"assistant","content":"","tool_calls":[{"id":"tc_1","function":{"name":"Read"}}]},
		{"role":"tool","tool_call_id":"tc_1","content":"R"},
		{"role":"user","content":[{"type":"text","text":"first failed retry"},{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD"}}]},
		{"role":"user","content":[{"type":"text","text":"latest current instruction"},{"type":"image_url","image_url":{"url":"data:image/png;base64,REVG"}}]}
	]}`))
	if inp["type"] != "tool_results" {
		t.Fatalf("unresolved retry transcript must remain a continuation, got %v", inp["type"])
	}
	if inp["userText"] != "latest current instruction" {
		t.Fatalf("only the latest unresolved user snapshot is current, got %q", inp["userText"])
	}
	imgs, _ := inp["images"].([]map[string]any)
	if len(imgs) != 1 || imgs[0]["data"] != "REVG" {
		t.Fatalf("only latest-snapshot images may be forwarded, got %#v", imgs)
	}
	if strings.Contains(inp["userText"].(string), "first failed retry") {
		t.Fatalf("failed retry history was concatenated into current intent: %q", inp["userText"])
	}
}

func TestComposerLegacyUnsignedContinuationCarriesFaithfulRecoveryReplay(t *testing.T) {
	oai := []byte(`{"messages":[
		{"role":"user","content":"start the implementation"},
		{"role":"assistant","tool_calls":[
			{"id":"call_old_a","type":"function","function":{"name":"todo","arguments":"{\"op\":\"view\"}"}},
			{"id":"call_old_b","type":"function","function":{"name":"job","arguments":"{\"action\":\"list\"}"}}
		]},
		{"role":"tool","tool_call_id":"call_old_a","content":"todo snapshot"},
		{"role":"tool","tool_call_id":"call_old_b","content":"subagent interrupted"},
		{"role":"user","content":"<system-notice>background task stopped</system-notice>"},
		{"role":"user","content":"resume every interrupted subagent"}
	]}`)
	hint := composerContinuationHintFor("", oai)
	inp := composerInputHinted(oai, hint)
	if inp["type"] != "tool_results" {
		t.Fatalf("legacy replay must still enter through /continue, got %#v", inp)
	}
	if inp["legacyUnsignedReplay"] != true {
		t.Fatalf("all-legacy result batch was not marked for bridge-owned recovery: %#v", inp)
	}
	if got := inp["userText"]; got != "resume every interrupted subagent" {
		t.Fatalf("latest recovery instruction = %#v", got)
	}
	if got, _ := inp["clientMessageId"].(string); !strings.HasPrefix(got, "ccm2_") {
		t.Fatalf("legacy recovery lacks deterministic request identity: %#v", inp)
	}
	history, _ := inp["history"].(string)
	for _, want := range []string{
		"call_old_a:todo", "call_old_b:job", "TOOL[call_old_a]: todo snapshot",
		"TOOL[call_old_b]: subagent interrupted", "background task stopped",
	} {
		if !strings.Contains(history, want) {
			t.Fatalf("legacy recovery history dropped %q: %s", want, history)
		}
	}
	if strings.Contains(history, "resume every interrupted subagent") {
		t.Fatalf("latest instruction must be sent once, not duplicated into recovery history: %s", history)
	}
}

func TestComposerSignedOrMixedContinuationNeverDowngradesToLegacyReplay(t *testing.T) {
	hint := composerContinuationHintFor("", nil)
	cases := map[string][]map[string]any{
		"signed": {
			{"toolCallId": composerClientToolRoutingTokenPrefix + "route_0_signature"},
		},
		"mixed": {
			{"toolCallId": "call_old"},
			{"toolCallId": composerClientToolRoutingTokenPrefix + "route_0_signature"},
		},
		"missing": {
			{"toolCallId": ""},
		},
	}
	for name, results := range cases {
		t.Run(name, func(t *testing.T) {
			if composerLegacyUnsignedResultReplay(results, hint) {
				t.Fatalf("%s batch must remain on strict signed validation: %#v", name, results)
			}
		})
	}
	if !composerLegacyUnsignedResultReplay([]map[string]any{{"toolCallId": "call_old"}}, hint) {
		t.Fatal("an all-legacy non-empty batch must be recoverable from faithful replay")
	}
}

func TestComposerLegacyResultOnlyContinuationReplaysEntireConversation(t *testing.T) {
	oai := []byte(`{"messages":[
		{"role":"user","content":"inspect the repository"},
		{"role":"assistant","tool_calls":[{"id":"call_old","type":"function","function":{"name":"Read","arguments":"{\"path\":\"README.md\"}"}}]},
		{"role":"tool","tool_call_id":"call_old","content":"repository overview"}
	]}`)
	inp := composerInputHinted(oai, composerContinuationHintFor("", oai))
	if inp["legacyUnsignedReplay"] != true {
		t.Fatalf("result-only legacy continuation was not recoverable: %#v", inp)
	}
	history, _ := inp["history"].(string)
	for _, want := range []string{"inspect the repository", "call_old:Read", "TOOL[call_old]: repository overview"} {
		if !strings.Contains(history, want) {
			t.Fatalf("result-only replay dropped %q: %s", want, history)
		}
	}
	if _, ok := inp["userText"]; ok {
		t.Fatalf("result-only continuation invented a user instruction: %#v", inp)
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
		// Same header conv id + same opener => stable session across turns.
		s2, _ := deriveComposerSessionID(auth, "cursorkey", toolTurn("hello"), o1)
		if s1 != s2 {
			t.Fatalf("header %s: same conv id + same opener must be stable (%s vs %s)", h, s1, s2)
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
		// Same body id + same opener => stable session across turns.
		s2, _ := deriveComposerSessionID(auth, "cursorkey", toolTurn("hi"), o)
		if s1 != s2 {
			t.Fatalf("body %s: same conv id + same opener must be stable (%s vs %s)", k, s1, s2)
		}
	}
	// H16: an unmapped previous_response_id may re-seed only when the request
	// carries bounded prior history. The unstable response id never enters the
	// content key, so two such faithful replays resolve identically.
	historyTurn := []byte(`{"tools":[{"type":"function","function":{"name":"Read"}}],"messages":[
		{"role":"user","content":"opener"},{"role":"assistant","content":"prior answer"},{"role":"user","content":"hi"}]}`)
	p1, err1 := deriveComposerSessionID(auth, "cursorkey", historyTurn,
		cliproxyexecutor.Options{OriginalRequest: []byte(`{"previous_response_id":"unmapped-prev-1"}`)})
	p2, err2 := deriveComposerSessionID(auth, "cursorkey", historyTurn,
		cliproxyexecutor.Options{OriginalRequest: []byte(`{"previous_response_id":"unmapped-prev-2"}`)})
	if err1 != nil || err2 != nil || p1 != p2 {
		t.Fatalf("H16: an unmapped previous_response_id must NOT enter the key — same opener must key the same regardless of response id: %s != %s", p1, p2)
	}
	if _, err := deriveComposerSessionID(auth, "cursorkey", toolTurn("thin follow-up"),
		cliproxyexecutor.Options{OriginalRequest: []byte(`{"previous_response_id":"unknown-legacy"}`)}); err == nil {
		t.Fatal("an unmapped thin previous_response_id follow-up must return 410 instead of starting blank")
	} else if statusErr, ok := err.(interface{ StatusCode() int }); !ok || statusErr.StatusCode() != http.StatusGone {
		t.Fatalf("thin continuity error must be typed 410, got %T %v", err, err)
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

// H10 (C-CONTINUATION-TOOLS): composerTurnBody must attach the authoritative tool snapshot on EVERY turn when
// advertised, not only on a new-user turn. The bridge refreshes its registry from toolInventoryJSON on
// tool_results turns too, so dropping the snapshot on a continuation left a stale advertise set. tool_choice=none
// sends an explicit empty snapshot so a reused bridge clears old tools.
func TestComposerTurnBodyToolsOnContinuation(t *testing.T) {
	adv := []map[string]any{{"name": "Read", "toolName": "Read"}}
	// A tool_results continuation MUST still carry the current tool inventory.
	cont := composerTurnBody("s1", "composer-2.5", map[string]any{"type": "tool_results", "results": []any{}}, adv, "auto", nil, nil, 0)
	if gjson.Parse(gjson.GetBytes(cont, "toolInventoryJSON").String()).Get("0.name").String() != "Read" {
		t.Fatalf("H10: continuation turn must include the current tools array, got %s", cont)
	}
	// A new-user turn still carries tools (unchanged behavior).
	user := composerTurnBody("s1", "composer-2.5", map[string]any{"type": "user", "text": "x"}, adv, "auto", nil, nil, 0)
	if gjson.Parse(gjson.GetBytes(user, "toolInventoryJSON").String()).Get("0.name").String() != "Read" {
		t.Fatalf("H10: user turn must include tools, got %s", user)
	}
	// tool_choice=none gating (advertise=nil) still wins and explicitly clears.
	none := composerTurnBody("s1", "composer-2.5", map[string]any{"type": "tool_results", "results": []any{}}, nil, "none", nil, nil, 0)
	if tools := gjson.Parse(gjson.GetBytes(none, "toolInventoryJSON").String()); !tools.IsArray() || len(tools.Array()) != 0 {
		t.Fatalf("H10: tool_choice=none must clear tools even on a continuation, got %s", none)
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
// hashed as a stable id. On a map miss, a thin request returns an honest 410.
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
	// New response ids are self-routing. Clear the compatibility cache to
	// model a full Go process restart and prove a Responses-only client still
	// reaches the exact durable session with no conversation id or history.
	routed := composerResponseID("cursorkey", tenant, prior)
	composerResponseSessionMu.Lock()
	savedSessions := composerResponseSessions
	savedOrder := composerResponseSessionOrder
	composerResponseSessions = make(map[string]string)
	composerResponseSessionOrder = nil
	composerResponseSessionMu.Unlock()
	defer func() {
		composerResponseSessionMu.Lock()
		composerResponseSessions = savedSessions
		composerResponseSessionOrder = savedOrder
		composerResponseSessionMu.Unlock()
	}()
	routedFollowup := cliproxyexecutor.Options{OriginalRequest: []byte(fmt.Sprintf(`{"previous_response_id":%q,"input":"after restart"}`, routed))}
	gotRouted, errRouted := deriveComposerSessionID(auth, "cursorkey", []byte(`{"messages":[{"role":"user","content":"after restart"}]}`), routedFollowup)
	if errRouted != nil || gotRouted != prior {
		t.Fatalf("self-routing previous_response_id must survive restart (id=%q err=%v), want %q", gotRouted, errRouted, prior)
	}
	// Unknown previous_response_id with no replay history must never start a blank agent.
	miss := cliproxyexecutor.Options{OriginalRequest: []byte(`{"previous_response_id":"chatcmpl-composer-UNKNOWN","input":"hi"}`)}
	gotMiss, errMiss := deriveComposerSessionID(auth, "cursorkey", []byte(`{"messages":[{"role":"user","content":"hi"}]}`), miss)
	if errMiss == nil {
		t.Fatalf("H16: unknown thin previous_response_id must return 410, got id=%q", gotMiss)
	}
	if statusErr, ok := errMiss.(interface{ StatusCode() int }); !ok || statusErr.StatusCode() != http.StatusGone {
		t.Fatalf("H16: unknown continuity error must be typed 410, got %T %v", errMiss, errMiss)
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
	// A realistic tool-less follow-up REPLAYS the same opener (the durable conversation grows at the tail).
	// identity-finalplan: the opener bridge re-keys the same lineage as the head grows, so the follow-up
	// resolves to the SAME stable session (the L35/H16 durable-resume contract).
	toolless := []byte(`{"conversation_id":"conv-l35-body","messages":[{"role":"user","content":"describe the repo"},{"role":"assistant","content":"it is a proxy"},{"role":"user","content":"summarize"}]}`)
	got, err := deriveComposerSessionID(auth, "cursorkey", toolless, bodyOpts)
	if err != nil {
		t.Fatalf("L35 follow-up must not error: %v", err)
	}
	if got != stable {
		t.Fatalf("L35/H16: a tool-less follow-up with a body conversation_id must route to the stable session %s, got %s", stable, got)
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
		composerExecOpts("openai", "redact1"))
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
// so a long/queued turn does not trip a client idle-abort while content waits for durable commit.
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
			composerExecOpts(fmtName, "ka-"+fmtName))
		if err != nil {
			t.Fatalf("%s: stream setup: %v", fmtName, err)
		}
		out := collect(sr)
		if !strings.Contains(out, ": keepalive") {
			t.Fatalf("%s: M24 keepalive comment not emitted (would be zero bytes -> idle abort): %q", fmtName, out)
		}
	}
}

// A pre-content ping emits only an ignorable comment. It deliberately does not
// open response.created/the first candidate before the durable commit point.
func TestExecuteComposerStreamKeepaliveBeforeEnvelopeIsCommentOnly(t *testing.T) {
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
		composerExecOpts("openai-response", "ka-pre"))
	if err != nil {
		t.Fatalf("stream setup: %v", err)
	}
	var first []byte
	for chunk := range sr.Chunks {
		if chunk.Err == nil && len(chunk.Payload) > 0 && first == nil {
			first = append([]byte(nil), chunk.Payload...)
		}
	}
	if !strings.HasPrefix(string(first), ": keepalive") || strings.Contains(string(first), "response.created") {
		t.Fatalf("pre-envelope keepalive must be a comment-only frame, got %q", string(first))
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
			opts := composerExecOpts("openai", "a59-"+mode)
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
		composerExecOpts("openai", "a68"))
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
	opts := composerExecOpts("openai", "a56x")
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
	// Without previous_response_id AND without a signed bridge tool id, the SAME shape must NOT be a continuation (it is
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

func TestADD65_ResponsesChainedContinuationIgnoresHistoricalToolRoutes(t *testing.T) {
	oai := []byte(`{"previous_response_id":"resp_latest","messages":[
		{"role":"user","content":"old request"},
		{"role":"tool","tool_call_id":"cct1_old_route_0_signature","content":"historical result"},
		{"role":"user","content":"intermediate request"},
		{"role":"tool","tool_call_id":"cct1_current_route_0_signature","content":"current result"},
		{"role":"user","content":"latest instruction"}
	]}`)
	inp := composerInput(oai)
	results, _ := inp["results"].([]map[string]any)
	if len(results) != 1 || results[0]["toolCallId"] != "cct1_current_route_0_signature" {
		t.Fatalf("only the current contiguous result batch may be forwarded, got %#v", results)
	}
	if inp["userText"] != "latest instruction" {
		t.Fatalf("latest chained user instruction was not preserved: %v", inp["userText"])
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
	if c, _ := results[0]["content"].(string); c != "A done" {
		t.Fatalf("ADD-36: trailing user text must not mutate the result receipt, got %q", c)
	}
	if id, _ := mixed["clientMessageId"].(string); !strings.HasPrefix(id, "ccm2_") {
		t.Fatalf("ADD-36: mixed user input needs a stable message id, got %q", id)
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
	// A real conversation_id is still stable for the same opener (regression guard for the surviving signal).
	convOpts := cliproxyexecutor.Options{OriginalRequest: []byte(`{"conversation_id":"conv-real"}`)}
	c1, _ := deriveComposerSessionID(auth, "cursorkey", toolTurn("same opener"), convOpts)
	c2, _ := deriveComposerSessionID(auth, "cursorkey", toolTurn("same opener"), convOpts)
	if c1 != c2 {
		t.Fatalf("ADD-78: conversation_id must remain stable for the same opener, got %s vs %s", c1, c2)
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
	// Same conv + same key + same opener => stable.
	aAgain, _ := deriveComposerSessionID(auth, "cursor-key-one", toolTurn("hi"), conv)
	if a != aAgain {
		t.Fatalf("ADD-79: same conv + same key + same opener must be stable, got %s vs %s", a, aAgain)
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
	body := composerTurnBody("s1", "composer-2.5", inp, nil, "", nil, composerConstraints(oai), 0)
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

	// Control: no result is mutated, regardless of whether another result succeeded.
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
	if c, _ := res2[0]["content"].(string); c != "OK OUTPUT" {
		t.Fatalf("ADD-89: non-error result must remain unmodified, got %q", c)
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
	_, argsUnsafe := mapComposerToolCall("Read", gjson.Parse(`9007199254740993`), defs, nil)
	if got := gjson.Get(argsUnsafe, "input").Raw; got != "9007199254740993" {
		t.Fatalf("ADD-104: Go must preserve an unsafe integer lexeme for the bridge's typed refusal, got %s", argsUnsafe)
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

// Comment 5: a Responses previous_response_id continuation (and a signed-tool-id continuation) whose
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
	// WITH a signed client-tool id hint, it is likewise recognized as a continuation without Go ownership.
	ownHint := composerContinuationHint{hasClientToolID: func(id string) bool { return id == "tc_resp" }}
	if lastUserTurnImageOnlyInvalid(msgs, ownHint) {
		t.Fatalf("Comment 5: a signed-tool-id continuation with a malformed trailing image must NOT be rejected as image-only")
	}
}

func TestComposerForceStoreTrue_OverridesStoreFalse(t *testing.T) {
	// Cursor Composer is inherently durable, so a client's store:false DEFAULT (e.g. pi/openai-completions) is
	// OVERRIDDEN to true rather than 400-rejected (supersedes ADD-94). store:true/absent normalize to true too,
	// so the request is internally consistent on the durable path.
	for _, in := range []string{
		`{"model":"composer-2.5","messages":[],"store":false}`,
		`{"store":true}`,
		`{"model":"composer-2.5"}`,
	} {
		out := composerForceStoreTrue([]byte(in))
		if v := gjson.GetBytes(out, "store"); !v.Exists() || !v.Bool() {
			t.Fatalf("store must be coerced to true (in=%s out=%s)", in, out)
		}
	}
}
