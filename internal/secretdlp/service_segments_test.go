package secretdlp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestServiceIdentifierFindingDoesNotMutateToolNameOrContentMention(t *testing.T) {
	gin.SetMode(gin.TestMode)
	toolName := "mcp__codebase-memory-mcp__manage_adr"
	svc := newSegmentPolicyTestService(t)
	svc.scanner = staticScanner{{Secret: toolName, RuleID: "high-entropy", Source: "test", Confidence: 0.99}}

	body := []byte(`{"model":"gpt-test","messages":[{"role":"user","content":"please call mcp__codebase-memory-mcp__manage_adr"}],"tools":[{"type":"function","function":{"name":"mcp__codebase-memory-mcp__manage_adr","parameters":{"type":"object"}}}]}`)
	c := newSecretDLPTestGinContext("/v1/chat/completions")

	redacted, session, err := svc.RedactGinPayload(c, body)
	if err != nil {
		t.Fatalf("RedactGinPayload(): %v", err)
	}
	if session != nil {
		t.Fatalf("session = %+v, want nil because identifier finding is rejected", session)
	}
	if string(redacted) != string(body) {
		t.Fatalf("redacted body = %q, want unchanged %q", redacted, body)
	}
}

func TestServiceCredentialShapedIdentifierStillRedactsContentMention(t *testing.T) {
	gin.SetMode(gin.TestMode)
	token := "tp-s8lnnc4nf0a0s296fb63ya9vqzvctz0ohk26q1ewrks0252f"
	svc := newSegmentPolicyTestService(t)

	body := []byte(`{"model":"gpt-test","messages":[{"role":"user","content":"please protect ` + token + `"}],"tools":[{"type":"function","function":{"name":"` + token + `","parameters":{"type":"object"}}}]}`)
	c := newSecretDLPTestGinContext("/v1/chat/completions")

	redacted, session, err := svc.RedactGinPayload(c, body)
	if err != nil {
		t.Fatalf("RedactGinPayload(): %v", err)
	}
	if session == nil {
		t.Fatal("session = nil, want session for credential-shaped content mention")
	}
	if got := strings.Count(string(redacted), token); got != 1 {
		t.Fatalf("redacted body contains raw token %d times, want only the tool name left: %s", got, redacted)
	}
	if !strings.Contains(string(redacted), `"name":"`+token+`"`) {
		t.Fatalf("redacted body mutated tool name: %s", redacted)
	}
	if !strings.Contains(string(redacted), `please protect __CPA_DLP_v1_`) {
		t.Fatalf("redacted body did not replace content mention with placeholder: %s", redacted)
	}
}

func TestServiceBuiltinEntropyOnOffDoesNotMutateIdentifierMention(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"gpt-test","messages":[{"role":"user","content":"please call mcp__codebase-memory-mcp__manage_adr"}],"tools":[{"type":"function","function":{"name":"mcp__codebase-memory-mcp__manage_adr","parameters":{"type":"object"}}}]}`)

	for _, highEntropy := range []bool{false, true} {
		t.Run(fmt.Sprintf("high_entropy_%v", highEntropy), func(t *testing.T) {
			svc := newSegmentPolicyTestService(t)
			svc.cfg.HighEntropy = highEntropy
			scanner, err := NewScanner(svc.cfg)
			if err != nil {
				t.Fatalf("NewScanner(): %v", err)
			}
			svc.scanner = scanner
			c := newSecretDLPTestGinContext("/v1/chat/completions")

			redacted, session, err := svc.RedactGinPayload(c, body)
			if err != nil {
				t.Fatalf("RedactGinPayload(): %v", err)
			}
			if session != nil {
				t.Fatalf("session = %+v, want nil because identifier mention is not a secret", session)
			}
			if string(redacted) != string(body) {
				t.Fatalf("redacted body = %q, want unchanged %q", redacted, body)
			}
		})
	}
}

func TestServiceRawFallbackOnlyRedactsExplicitSecretShapes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := newSegmentPolicyTestService(t)
	pem := "-----BEGIN PRIVATE KEY-----\nabc123SECRET\n-----END PRIVATE KEY-----"
	toolName := "GetUserProfileWithOrdersAndRecommendations"
	body := []byte("not-json " + toolName + " " + pem)
	c := newSecretDLPTestGinContext("/v1/chat/completions")

	redacted, session, err := svc.RedactGinPayload(c, body)
	if err != nil {
		t.Fatalf("RedactGinPayload(): %v", err)
	}
	if session == nil {
		t.Fatal("session = nil, want session for PEM redaction")
	}
	if bytes.Contains(redacted, []byte(pem)) {
		t.Fatalf("redacted body still contains PEM: %q", redacted)
	}
	if !bytes.Contains(redacted, []byte(toolName)) {
		t.Fatalf("redacted body = %q, want benign identifier untouched", redacted)
	}
}

func TestServiceUnknownRouteUsesRawExplicitFallbackOnly(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := newSegmentPolicyTestService(t)
	opName := "GetUserProfileWithOrdersAndRecommendations"
	body := []byte(`{"query":"` + opName + `"}`)
	c := newSecretDLPTestGinContext("/v1/unknown")

	redacted, session, err := svc.RedactGinPayload(c, body)
	if err != nil {
		t.Fatalf("RedactGinPayload(): %v", err)
	}
	if session != nil {
		t.Fatalf("session = %+v, want nil for fallback without explicit secrets", session)
	}
	if string(redacted) != string(body) {
		t.Fatalf("redacted body = %q, want unchanged %q", redacted, body)
	}
}

func TestServiceDoesNotRedactPositiveListExcludedSecretField(t *testing.T) {
	gin.SetMode(gin.TestMode)
	secret := "sk-testdlpfixture0000000000000000000000000000"
	svc := newSegmentPolicyTestService(t)
	svc.scanner = staticScanner{{Secret: secret, RuleID: "manual", Source: "test", Confidence: 0.99}}

	body := []byte(`{"api_key":"` + secret + `"}`)
	c := newSecretDLPTestGinContext("/v1/chat/completions")

	redacted, session, err := svc.RedactGinPayload(c, body)
	if err != nil {
		t.Fatalf("RedactGinPayload(): %v", err)
	}
	if session != nil {
		t.Fatalf("session = %+v, want nil because top-level api_key is outside the route allowlist", session)
	}
	if string(redacted) != string(body) {
		t.Fatalf("redacted body = %q, want unchanged %q", redacted, body)
	}
}

func TestServiceShortSchemaKeyDoesNotSuppressRealSecret(t *testing.T) {
	gin.SetMode(gin.TestMode)
	secret := "sk-testdlpfixture0000000000000000000000000000"
	svc := newSegmentPolicyTestService(t)

	body := []byte(`{"model":"gpt-test","messages":[{"role":"user","content":"use ` + secret + `"}],"tools":[{"type":"function","function":{"name":"Lookup","parameters":{"type":"object","properties":{"sk":{"type":"string"}}}}}]}`)
	c := newSecretDLPTestGinContext("/v1/chat/completions")

	redacted, session, err := svc.RedactGinPayload(c, body)
	if err != nil {
		t.Fatalf("RedactGinPayload(): %v", err)
	}
	if session == nil {
		t.Fatal("session = nil, want session for real secret redaction")
	}
	if bytes.Contains(redacted, []byte(secret)) {
		t.Fatalf("redacted body still contains real secret despite short schema key: %q", redacted)
	}
	if !bytes.Contains(redacted, []byte(`"sk":{"type":"string"}`)) {
		t.Fatalf("redacted body = %q, want short schema key preserved", redacted)
	}
}

func TestServiceWholeObjectDetectorDoesNotMutateToolSchema(t *testing.T) {
	gin.SetMode(gin.TestMode)
	privateKey := "-----BEGIN PRIVATE KEY-----\\nabc123fixture"
	gcpJSON := `{"type":"service_account","project_id":"p","private_key":"` + privateKey + `"}`
	content, err := json.Marshal(gcpJSON)
	if err != nil {
		t.Fatalf("json.Marshal(content): %v", err)
	}
	description, err := json.Marshal(gcpJSON)
	if err != nil {
		t.Fatalf("json.Marshal(description): %v", err)
	}
	svc := newSegmentPolicyTestService(t)

	body := []byte(`{"model":"gpt-test","messages":[{"role":"user","content":` + string(content) + `}],"tools":[{"type":"function","function":{"name":"Lookup","description":` + string(description) + `,"parameters":{"type":"object"}}}]}`)
	c := newSecretDLPTestGinContext("/v1/chat/completions")

	redacted, session, err := svc.RedactGinPayload(c, body)
	if err != nil {
		t.Fatalf("RedactGinPayload(): %v", err)
	}
	if session == nil {
		t.Fatal("session = nil, want session for content service-account key redaction")
	}
	if got := strings.Count(string(redacted), "-----BEGIN PRIVATE KEY-----"); got != 1 {
		t.Fatalf("redacted body contains private key %d times, want only tool schema copy left: %s", got, redacted)
	}
	if !bytes.Contains(redacted, description) {
		t.Fatalf("redacted body mutated tool description: %s", redacted)
	}
	if !bytes.Contains(redacted, []byte("__CPA_DLP_v1_")) {
		t.Fatalf("redacted body missing placeholder: %s", redacted)
	}
}

func TestServiceRedactsAnthropicToolInputSecretWithKeyContext(t *testing.T) {
	gin.SetMode(gin.TestMode)
	secret := "aB9xK2mN7pQ5rT8z"
	svc := newSegmentPolicyTestService(t)

	body := []byte(`{"model":"claude-test","messages":[{"role":"user","content":[{"type":"tool_use","id":"toolu_1","name":"login","input":{"username":"jeff","password":"` + secret + `"}}]}]}`)
	c := newSecretDLPTestGinContext("/v1/messages")

	redacted, session, err := svc.RedactGinPayload(c, body)
	if err != nil {
		t.Fatalf("RedactGinPayload(): %v", err)
	}
	if session == nil {
		t.Fatal("session = nil, want session for tool input password redaction")
	}
	if bytes.Contains(redacted, []byte(secret)) {
		t.Fatalf("redacted body still contains tool input secret: %q", redacted)
	}
	if !bytes.Contains(redacted, []byte(`"password"`)) {
		t.Fatalf("redacted body = %q, want password key preserved", redacted)
	}
	if !bytes.Contains(redacted, []byte(`"username":"jeff"`)) {
		t.Fatalf("redacted body = %q, want sibling non-secret value preserved", redacted)
	}
	if !bytes.Contains(redacted, []byte("__CPA_DLP_v1_")) {
		t.Fatalf("redacted body missing placeholder: %q", redacted)
	}
}

func TestServicePassthroughPlaceholderRestoresOnlySameClientFromStore(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := newSegmentPolicyTestService(t)
	secret := "sk-testdlpfixture0000000000000000000000000000"
	originalSession := NewSession([]byte("master-key"), "cliproxy-client-key", time.Hour, ModeRestore)
	redacted := redactRawForTest(t, originalSession, []byte(`{"message":"`+secret+`"}`), []Finding{{Secret: secret, RuleID: "manual", Source: "test"}})
	placeholder := extractPlaceholderForTest(t, string(redacted))
	if err := svc.persistMappings(context.Background(), originalSession, []Mapping{{Placeholder: placeholder, Secret: []byte(secret)}}); err != nil {
		t.Fatalf("persistMappings(): %v", err)
	}

	body := []byte(`{"messages":[{"role":"user","content":"repeat ` + placeholder + `"}]}`)
	c := newSecretDLPTestGinContext("/v1/chat/completions")
	passed, session, err := svc.RedactGinPayload(c, body)
	if err != nil {
		t.Fatalf("RedactGinPayload(): %v", err)
	}
	if session == nil {
		t.Fatal("session = nil, want passthrough session for stored placeholder")
	}
	if session.ClientID != originalSession.ClientID {
		t.Fatalf("passthrough session client_id = %q, want %q", session.ClientID, originalSession.ClientID)
	}
	if string(passed) != string(body) {
		t.Fatalf("passed body = %q, want unchanged %q", passed, body)
	}

	restored := svc.RestoreResponse(c.Request.Context(), []byte(`{"message":"`+placeholder+`"}`))
	if !strings.Contains(string(restored), secret) {
		t.Fatalf("RestoreResponse() = %q, want same-client placeholder restored", restored)
	}

	clientless := svc.RestoreResponse(context.Background(), []byte(`{"message":"`+placeholder+`"}`))
	if strings.Contains(string(clientless), secret) {
		t.Fatalf("clientless RestoreResponse() = %q, want no unscoped restore", clientless)
	}

	otherClient := NewSession([]byte("master-key"), "other-client-key", time.Hour, ModeRestore)
	other := svc.RestoreResponse(WithSession(context.Background(), otherClient), []byte(`{"message":"`+placeholder+`"}`))
	if strings.Contains(string(other), secret) {
		t.Fatalf("other-client RestoreResponse() = %q, want no cross-client restore", other)
	}
}

func TestServiceRestoresOnlySameClientPlaceholderFromStore(t *testing.T) {
	svc := newSegmentPolicyTestService(t)
	clientA := NewSession([]byte("master-key"), "client-a", time.Hour, ModeRestore)
	clientB := NewSession([]byte("master-key"), "client-b", time.Hour, ModeRestore)
	secret := "sk-testdlpfixture0000000000000000000000000000"
	redacted := redactRawForTest(t, clientA, []byte(`{"message":"`+secret+`"}`), []Finding{{Secret: secret, RuleID: "manual", Source: "test"}})
	placeholder := extractPlaceholderForTest(t, string(redacted))
	if err := svc.persistMappings(context.Background(), clientA, []Mapping{{Placeholder: placeholder, Secret: []byte(secret)}}); err != nil {
		t.Fatalf("persistMappings(): %v", err)
	}

	ctxB := WithSession(context.Background(), clientB)
	restored := svc.RestoreResponse(ctxB, []byte(`{"message":"`+placeholder+`"}`))
	if strings.Contains(string(restored), secret) {
		t.Fatalf("RestoreResponse() = %q, want client B not to restore client A placeholder", restored)
	}

	ctxA := WithSession(context.Background(), clientA)
	restored = svc.RestoreResponse(ctxA, []byte(`{"message":"`+placeholder+`"}`))
	if !strings.Contains(string(restored), secret) {
		t.Fatalf("RestoreResponse() = %q, want client A to restore own placeholder", restored)
	}
}

func newSegmentPolicyTestService(t *testing.T) *Service {
	t.Helper()
	svc, err := New(Config{
		Enabled:         true,
		Mode:            ModeRestore,
		MasterKey:       []byte("master-key"),
		TTL:             time.Hour,
		MaxFindings:     10,
		MinValueLength:  12,
		Scanner:         "builtin",
		Store:           storeMemory,
		RedactThreshold: 0.80,
	})
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	return svc
}

func newSecretDLPTestGinContext(path string) *gin.Context {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, path, nil)
	c.Request.Header.Set("Authorization", "Bearer cliproxy-client-key")
	return c
}
