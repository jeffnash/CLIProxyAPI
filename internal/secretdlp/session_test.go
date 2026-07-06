package secretdlp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestSessionRedactsAndRestoresJSON(t *testing.T) {
	session := NewSession([]byte("master-key"), "client-key", time.Minute, ModeRestore)
	secret := "sk-testdlpfixture0000000000000000000000000000"
	body := []byte(`{"message":"use sk-testdlpfixture0000000000000000000000000000 here"}`)

	redacted := redactRawForTest(t, session, body, []Finding{{Secret: secret, RuleID: "test", Source: "test"}})
	if bytes.Contains(redacted, []byte(secret)) {
		t.Fatalf("redacted body still contains secret: %q", redacted)
	}
	if !bytes.Contains(redacted, []byte("__CPA_DLP_v1_")) {
		t.Fatalf("redacted body missing placeholder: %q", redacted)
	}

	restored := session.RestoreJSON(redacted)
	if string(restored) != string(body) {
		t.Fatalf("restored body = %q, want %q", restored, body)
	}
}

func TestSessionDoesNotRedactToolNamesOrSchemas(t *testing.T) {
	session := NewSession([]byte("master-key"), "client-key", time.Minute, ModeRestore)
	secret := "sk-THL5JPzaa0VUlLJ7rZYrn78C6GHjrA2kDAPWnlCSWGFzy3T2"
	body := []byte(`{"tools":[{"type":"function","function":{"name":"` + secret + `","description":"` + secret + `","parameters":{"type":"object","properties":{"api_key":{"description":"` + secret + `"}}}}}]}`)
	doc, err := tokenizeJSON(body)
	if err != nil {
		t.Fatalf("tokenizeJSON(): %v", err)
	}
	segments := extractSegments(doc, packForRoute("/v1/chat/completions"))

	redacted, mappings := session.RedactSegments(body, segments, []Finding{{Secret: secret, RuleID: "manual", Source: "test"}})
	if len(mappings) != 0 {
		t.Fatalf("mappings = %+v, want none for tool schema-only finding", mappings)
	}
	if string(redacted) != string(body) {
		t.Fatalf("redacted tool schema = %q, want unchanged %q", redacted, body)
	}
}

func TestSessionDoesNotRedactSchemaPropertyKeyFinding(t *testing.T) {
	session := NewSession([]byte("master-key"), "client-key", time.Minute, ModeRestore)
	secret := "sk-PHL5JPzaa0VUlLJ7rZYrn78C6GHjrA2kDAPWnlCSWGFzy3P0"
	body := []byte(`{"tools":[{"type":"function","function":{"name":"Lookup","parameters":{"type":"object","properties":{"` + secret + `":{"type":"string"}}}}}]}`)
	doc, err := tokenizeJSON(body)
	if err != nil {
		t.Fatalf("tokenizeJSON(): %v", err)
	}
	segments := extractSegments(doc, packForRoute("/v1/chat/completions"))

	redacted, mappings := session.RedactSegments(body, segments, []Finding{{Secret: secret, RuleID: "manual", Source: "test"}})
	if len(mappings) != 0 {
		t.Fatalf("mappings = %+v, want none for schema property key finding", mappings)
	}
	if string(redacted) != string(body) {
		t.Fatalf("redacted schema property key = %q, want unchanged %q", redacted, body)
	}
}

func TestSessionDoesNotRedactUnknownJSONFieldsByDefault(t *testing.T) {
	session := NewSession([]byte("master-key"), "client-key", time.Minute, ModeRestore)
	secret := "sk-testdlpfixture0000000000000000000000000000"
	body := []byte(`{"trace":"` + secret + `"}`)
	doc, err := tokenizeJSON(body)
	if err != nil {
		t.Fatalf("tokenizeJSON(): %v", err)
	}
	segments := extractSegments(doc, packForRoute("/v1/chat/completions"))

	redacted, mappings := session.RedactSegments(body, segments, []Finding{{Secret: secret, RuleID: "manual", Source: "test"}})
	if len(mappings) != 0 {
		t.Fatalf("mappings = %+v, want none for unknown request field", mappings)
	}
	if string(redacted) != string(body) {
		t.Fatalf("redacted unknown field = %q, want unchanged %q", redacted, body)
	}
}

func TestSessionRedactsMessageContentAndToolArgumentsOnly(t *testing.T) {
	session := NewSession([]byte("master-key"), "client-key", time.Minute, ModeRestore)
	secret := "sk-testdlpfixture0000000000000000000000000000"
	keySecret := "sk-KHL5JPzaa0VUlLJ7rZYrn78C6GHjrA2kDAPWnlCSWGFzy3K0"
	body := []byte(`{
		"messages":[{
			"role":"assistant",
			"content":"echo sk-testdlpfixture0000000000000000000000000000",
			"tool_calls":[{
				"id":"call_1",
				"type":"function",
				"function":{
					"name":"Lookup",
					"arguments":"{  \"sk-KHL5JPzaa0VUlLJ7rZYrn78C6GHjrA2kDAPWnlCSWGFzy3K0\" : \"leave-key\",  \"api_key\" : \"sk-testdlpfixture0000000000000000000000000000\"  }"
				}
			}]
		}],
		"tools":[{
			"type":"function",
			"function":{
				"name":"Lookup",
				"description":"tool description mentions sk-testdlpfixture0000000000000000000000000000",
				"parameters":{"type":"object","properties":{"api_key":{"description":"sk-testdlpfixture0000000000000000000000000000"}}}
			}
		}]
	}`)

	doc, err := tokenizeJSON(body)
	if err != nil {
		t.Fatalf("tokenizeJSON(): %v", err)
	}
	segments := extractSegments(doc, packForRoute("/v1/chat/completions"))
	redacted, mappings := session.RedactSegments(body, segments, []Finding{
		{Secret: secret, RuleID: "manual", Source: "test"},
		{Secret: keySecret, RuleID: "manual", Source: "test"},
	})
	if len(mappings) != 1 {
		t.Fatalf("mappings = %+v, want one", mappings)
	}
	placeholder := extractPlaceholderForTest(t, string(redacted))
	root := decodeJSONForTest(t, redacted)

	if got := root["messages"].([]any)[0].(map[string]any)["content"].(string); !strings.Contains(got, placeholder) {
		t.Fatalf("message content = %q, want placeholder %q", got, placeholder)
	}
	args := root["messages"].([]any)[0].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)["arguments"].(string)
	if !strings.Contains(args, placeholder) {
		t.Fatalf("tool call arguments = %q, want placeholder %q", args, placeholder)
	}
	if !strings.Contains(args, keySecret) {
		t.Fatalf("tool call arguments = %q, want nested key %q untouched", args, keySecret)
	}
	wantArgs := `{  "` + keySecret + `" : "leave-key",  "api_key" : "` + placeholder + `"  }`
	if args != wantArgs {
		t.Fatalf("tool call arguments = %q, want whitespace/order-preserving %q", args, wantArgs)
	}

	toolFn := root["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if got := toolFn["name"].(string); got != "Lookup" {
		t.Fatalf("tool name = %q, want unchanged", got)
	}
	if got := toolFn["description"].(string); !strings.Contains(got, secret) {
		t.Fatalf("tool description = %q, want original secret left unchanged", got)
	}
	params := toolFn["parameters"].(map[string]any)["properties"].(map[string]any)["api_key"].(map[string]any)["description"].(string)
	if !strings.Contains(params, secret) {
		t.Fatalf("schema description = %q, want original secret left unchanged", params)
	}
}

func TestSessionRedactSegmentsEditsOnlyStringToken(t *testing.T) {
	session := NewSession([]byte("master-key"), "client-key", time.Minute, ModeRestore)
	secret := "sk-testdlpfixture0000000000000000000000000000"
	body := []byte(`{"model":"gpt-test","messages":[{"role":"user","content":"before sk-testdlpfixture0000000000000000000000000000 after"}],"tools":[{"type":"function","function":{"name":"Lookup","parameters":{"type":"object"}}}]}`)
	doc, err := tokenizeJSON(body)
	if err != nil {
		t.Fatalf("tokenizeJSON(): %v", err)
	}
	segments := extractSegments(doc, packForRoute("/v1/chat/completions"))
	redacted, mappings := session.RedactSegments(body, segments, []Finding{{Secret: secret, RuleID: "manual", Source: "test"}})
	if len(mappings) != 1 {
		t.Fatalf("mappings = %+v, want one", mappings)
	}
	placeholder := extractPlaceholderForTest(t, string(redacted))

	var content Segment
	for _, seg := range segments {
		if seg.Path == "/messages/0/content" {
			content = seg
			break
		}
	}
	if content.Path == "" {
		t.Fatalf("segments = %+v, want /messages/0/content", segments)
	}
	expectedToken, err := json.Marshal("before " + placeholder + " after")
	if err != nil {
		t.Fatalf("json.Marshal(): %v", err)
	}
	expected := append([]byte{}, body[:content.TokSpan[0]]...)
	expected = append(expected, expectedToken...)
	expected = append(expected, body[content.TokSpan[1]:]...)
	if string(redacted) != string(expected) {
		t.Fatalf("redacted body = %q, want only content token edit %q", redacted, expected)
	}
}

func TestSessionRestoresUnknownResponsePayloadFields(t *testing.T) {
	session := NewSession([]byte("master-key"), "client-key", time.Minute, ModeRestore)
	secret := "sk-testdlpfixture0000000000000000000000000000"
	redacted := redactRawForTest(t, session, []byte(`{"message":"sk-testdlpfixture0000000000000000000000000000"}`), []Finding{{Secret: secret, RuleID: "manual", Source: "test"}})
	placeholder := extractPlaceholderForTest(t, string(redacted))

	restored := session.RestoreJSON([]byte(`{"echo":"` + placeholder + `"}`))
	if !strings.Contains(string(restored), secret) || strings.Contains(string(restored), placeholder) {
		t.Fatalf("restored response = %q, want secret restored and placeholder removed", restored)
	}
}

func TestSessionRestoresAdjacentPlaceholders(t *testing.T) {
	session := NewSession([]byte("master-key"), "client-key", time.Minute, ModeRestore)
	secretA := "sk-testdlpfixture0000000000000000000000000000"
	secretB := "sk-BHL5JPzaa0VUlLJ7rZYrn78C6GHjrA2kDAPWnlCSWGFzy3T1"
	redacted := redactRawForTest(t, session, []byte(`{"message":"sk-testdlpfixture0000000000000000000000000000 sk-BHL5JPzaa0VUlLJ7rZYrn78C6GHjrA2kDAPWnlCSWGFzy3T1"}`), []Finding{
		{Secret: secretA, RuleID: "manual", Source: "test"},
		{Secret: secretB, RuleID: "manual", Source: "test"},
	})
	placeholders := placeholderPattern.FindAllString(string(redacted), -1)
	if len(placeholders) != 2 {
		t.Fatalf("placeholders = %+v, want two", placeholders)
	}

	restored := session.RestoreJSON([]byte(`{"message":"` + placeholders[0] + placeholders[1] + `"}`))
	if !strings.Contains(string(restored), secretA+secretB) {
		t.Fatalf("restored adjacent placeholders = %q, want %q", restored, secretA+secretB)
	}
}

func TestSessionLeavesFabricatedPlaceholderUnchanged(t *testing.T) {
	session := NewSession([]byte("master-key"), "client-key", time.Minute, ModeRestore)
	fabricated := "__CPA_DLP_v1_AAAAAAAAAAAA_BBBBBBBBBBBBBBBB_001_CCCCCCCCCCC__"

	restored, count := session.RestoreJSONWithResolverStats([]byte(`{"message":"`+fabricated+`"}`), nil)
	if count != 0 {
		t.Fatalf("restored count = %d, want 0", count)
	}
	if !strings.Contains(string(restored), fabricated) {
		t.Fatalf("restored body = %q, want fabricated placeholder unchanged", restored)
	}
}

func TestSessionRestoresJSONContentButNotToolNames(t *testing.T) {
	session := NewSession([]byte("master-key"), "client-key", time.Minute, ModeRestore)
	secret := "sk-testdlpfixture0000000000000000000000000000"
	redacted := redactRawForTest(t, session, []byte(`{"message":"sk-testdlpfixture0000000000000000000000000000"}`), []Finding{{Secret: secret, RuleID: "manual", Source: "test"}})
	placeholder := extractPlaceholderForTest(t, string(redacted))

	response := []byte(`{
		"choices":[{
			"message":{
				"content":"` + placeholder + `",
				"tool_calls":[{
					"type":"function",
					"function":{"name":"` + placeholder + `","arguments":"{\"api_key\":\"` + placeholder + `\"}"}
				}]
			}
		}],
		"tools":[{"type":"function","function":{"name":"` + placeholder + `"}}]
	}`)
	restored := session.RestoreJSON(response)
	root := decodeJSONForTest(t, restored)

	message := root["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if got := message["content"].(string); got != secret {
		t.Fatalf("message content = %q, want restored secret", got)
	}
	args := message["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)["arguments"].(string)
	if !strings.Contains(args, secret) || strings.Contains(args, placeholder) {
		t.Fatalf("tool arguments = %q, want restored secret and no placeholder", args)
	}
	name := message["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)["name"].(string)
	if name != placeholder {
		t.Fatalf("tool call name = %q, want placeholder left untouched", name)
	}
	toolName := root["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)["name"].(string)
	if toolName != placeholder {
		t.Fatalf("schema tool name = %q, want placeholder left untouched", toolName)
	}
}

func TestSessionRestoresJSONEscapedSecret(t *testing.T) {
	session := NewSession([]byte("master-key"), "client-key", time.Minute, ModeRestore)
	secret := "line1\nline2\"quoted\""
	redacted := redactRawForTest(t, session, []byte("raw "+secret), []Finding{{Secret: secret, RuleID: "test", Source: "test"}})
	placeholder := extractPlaceholderForTest(t, string(redacted))

	restored := session.RestoreJSON([]byte(`{"message":"` + placeholder + `"}`))
	want := `{"message":"line1\nline2\"quoted\""}`
	if string(restored) != want {
		t.Fatalf("restored body = %q, want %q", restored, want)
	}
}

func TestSessionStreamRestoreHandlesSplitPlaceholder(t *testing.T) {
	session := NewSession([]byte("master-key"), "client-key", time.Minute, ModeRestore)
	secret := "sk-testdlpfixture0000000000000000000000000000"
	redacted := redactRawForTest(t, session, []byte(`{"message":"sk-testdlpfixture0000000000000000000000000000"}`), []Finding{{Secret: secret, RuleID: "test", Source: "test"}})
	placeholder := extractPlaceholderForTest(t, string(redacted))

	first := []byte(`data: {"delta":"before ` + placeholder[:len(placeholder)/2])
	second := []byte(placeholder[len(placeholder)/2:] + ` after"}` + "\n\n")

	var out []byte
	out = append(out, session.RestoreStreamJSONChunk(first)...)
	out = append(out, session.RestoreStreamJSONChunk(second)...)
	out = append(out, session.FlushStreamJSONTail()...)

	if strings.Contains(string(out), placeholder) {
		t.Fatalf("stream output still contains placeholder: %q", out)
	}
	if !strings.Contains(string(out), secret) {
		t.Fatalf("stream output = %q, want restored secret %q", out, secret)
	}
}

func TestSessionFreshRestoreStreamHandlesSplitPlaceholderWithResolver(t *testing.T) {
	source := NewSession([]byte("master-key"), "client-key", time.Minute, ModeRestore)
	restoreOnly := NewSession([]byte("master-key"), "client-key", time.Minute, ModeRestore)
	secret := "sk-RHL5JPzaa0VUlLJ7rZYrn78C6GHjrA2kDAPWnlCSWGFzy3R0"
	redacted := redactRawForTest(t, source, []byte(`{"message":"`+secret+`"}`), []Finding{{Secret: secret, RuleID: "test", Source: "test"}})
	placeholder := extractPlaceholderForTest(t, string(redacted))

	resolver := func(candidate string) ([]byte, bool) {
		if candidate != placeholder {
			return nil, false
		}
		return []byte(secret), true
	}
	first := []byte(`data: {"delta":"before ` + placeholder[:len(placeholder)/2])
	second := []byte(placeholder[len(placeholder)/2:] + ` after"}` + "\n\n")

	var out []byte
	out = append(out, restoreOnly.RestoreStreamJSONChunkWithResolver(first, resolver)...)
	out = append(out, restoreOnly.RestoreStreamJSONChunkWithResolver(second, resolver)...)
	out = append(out, restoreOnly.FlushStreamJSONTailWithResolver(resolver)...)

	if strings.Contains(string(out), placeholder) {
		t.Fatalf("fresh restore-only stream output still contains placeholder: %q", out)
	}
	if !strings.Contains(string(out), secret) {
		t.Fatalf("fresh restore-only stream output = %q, want restored secret %q", out, secret)
	}
}

func TestSessionPlaceholderPatternMatchesLargeCountersAndRestores(t *testing.T) {
	session := NewSession([]byte("master-key"), "client-key", time.Minute, ModeRestore)

	var lastSecret string
	var lastPlaceholder string
	for i := 1; i <= 1001; i++ {
		lastSecret = fmt.Sprintf("large-session-secret-%04d", i)
		redacted := redactRawForTest(t, session, []byte(lastSecret), []Finding{{Secret: lastSecret, RuleID: "test", Source: "test"}})
		lastPlaceholder = extractPlaceholderForTest(t, string(redacted))
		if !placeholderPattern.MatchString(lastPlaceholder) {
			t.Fatalf("placeholder %d = %q does not match placeholderPattern", i, lastPlaceholder)
		}
	}
	if !strings.Contains(lastPlaceholder, "_1001_") {
		t.Fatalf("last placeholder = %q, want counter 1001", lastPlaceholder)
	}

	restored := session.RestoreJSON([]byte(`{"message":"` + lastPlaceholder + `"}`))
	if !strings.Contains(string(restored), lastSecret) {
		t.Fatalf("restored large-counter placeholder = %q, want secret %q", restored, lastSecret)
	}
}

func TestSessionStreamingRestoreDocumentsBlockedPathBehavior(t *testing.T) {
	session := NewSession([]byte("master-key"), "client-key", time.Minute, ModeRestore)
	secret := "sk-SHL5JPzaa0VUlLJ7rZYrn78C6GHjrA2kDAPWnlCSWGFzy3S0"
	redacted := redactRawForTest(t, session, []byte(`{"message":"`+secret+`"}`), []Finding{{Secret: secret, RuleID: "test", Source: "test"}})
	placeholder := extractPlaceholderForTest(t, string(redacted))

	structured := session.RestoreJSON([]byte(`{"tools":[{"type":"function","function":{"name":"` + placeholder + `"}}]}`))
	if !strings.Contains(string(structured), placeholder) || strings.Contains(string(structured), secret) {
		t.Fatalf("structured restore = %q, want blocked tool name placeholder left unchanged", structured)
	}

	streamed := session.RestoreStreamJSONChunk([]byte(`data: {"tools":[{"type":"function","function":{"name":"` + placeholder + `"}}]}` + "\n\n"))
	if strings.Contains(string(streamed), placeholder) || !strings.Contains(string(streamed), secret) {
		t.Fatalf("streaming restore = %q, want raw placeholder replacement in streamed chunk", streamed)
	}
}

func TestSessionRedactForLogHandlesRawAndEscapedSecret(t *testing.T) {
	session := NewSession([]byte("master-key"), "client-key", time.Minute, ModeRestore)
	rawSecret := "line1\nline2\"quoted\""
	redacted := redactRawForTest(t, session, []byte("raw "+rawSecret), []Finding{{Secret: rawSecret, RuleID: "test", Source: "test"}})
	placeholder := extractPlaceholderForTest(t, string(redacted))

	logBody := []byte("raw: line1\nline2\"quoted\"\njson: line1\\nline2\\\"quoted\\\"")
	safe := session.RedactForLog(logBody)
	if bytes.Contains(safe, []byte(rawSecret)) || bytes.Contains(safe, []byte(`line1\nline2\"quoted\"`)) {
		t.Fatalf("log body still contains secret: %q", safe)
	}
	if !bytes.Contains(safe, []byte(placeholder)) {
		t.Fatalf("log body missing placeholder %q: %q", placeholder, safe)
	}
}

func TestSessionRawRedactionSkipsEmbeddedSecretValue(t *testing.T) {
	session := NewSession([]byte("master-key"), "client-key", time.Minute, ModeRestore)
	secret := "sk-testdlpfixture0000000000000000000000000000"
	body := []byte(`prefix` + secret + `suffix ` + secret)

	redacted, mappings := session.RedactRawWithMappings(body, []Finding{{Secret: secret, RuleID: "test", Source: "test"}})
	if len(mappings) != 1 {
		t.Fatalf("mappings = %+v, want one mapping for standalone secret", mappings)
	}
	if !strings.Contains(string(redacted), `prefix`+secret+`suffix`) {
		t.Fatalf("redacted body = %q, want embedded secret left unchanged", redacted)
	}
	if strings.Contains(string(redacted), ` `+secret) {
		t.Fatalf("redacted body = %q, want standalone secret replaced", redacted)
	}
}

func TestSessionRedactForLogSkipsEmbeddedSecretValue(t *testing.T) {
	session := NewSession([]byte("master-key"), "client-key", time.Minute, ModeRestore)
	secret := "sk-DHL5JPzaa0VUlLJ7rZYrn78C6GHjrA2kDAPWnlCSWGFzy3S0"
	redacted := redactRawForTest(t, session, []byte(secret), []Finding{{Secret: secret, RuleID: "test", Source: "test"}})
	placeholder := extractPlaceholderForTest(t, string(redacted))

	logBody := []byte(`prefix` + secret + `suffix standalone ` + secret)
	safe, count := session.RedactForLogStats(logBody)
	if count != 1 {
		t.Fatalf("redacted count = %d, want one standalone replacement", count)
	}
	if !strings.Contains(string(safe), `prefix`+secret+`suffix`) {
		t.Fatalf("log body = %q, want embedded secret left unchanged", safe)
	}
	if strings.Contains(string(safe), ` `+secret) {
		t.Fatalf("log body = %q, want standalone secret replaced", safe)
	}
	if !strings.Contains(string(safe), placeholder) {
		t.Fatalf("log body = %q, want placeholder %q", safe, placeholder)
	}
}

func redactRawForTest(t *testing.T, session *Session, body []byte, findings []Finding) []byte {
	t.Helper()
	redacted, _ := session.RedactRawWithMappings(body, findings)
	return redacted
}

func extractPlaceholderForTest(t *testing.T, body string) string {
	t.Helper()
	placeholder := placeholderPattern.FindString(body)
	if placeholder == "" {
		t.Fatalf("body %q does not contain DLP placeholder", body)
	}
	return placeholder
}

func decodeJSONForTest(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		t.Fatalf("json.Unmarshal(%q): %v", body, err)
	}
	return root
}
