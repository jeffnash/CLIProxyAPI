package handlers

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/secretdlp"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestExecuteWithAuthManagerRestoresSecretDLPResponse(t *testing.T) {
	const model = "secret-dlp-nonstream-model"
	const secret = "sk-testdlpfixture0000000000000000000000000000"

	session, redactedRequest, placeholder := newSecretDLPTestSession(t, model, secret)
	executor := &modelExecutionCaptureExecutor{
		execute: func(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
			return coreexecutor.Response{Payload: []byte(fmt.Sprintf(`{"echo":"%s"}`, placeholder))}, nil
		},
	}
	handler := newModelExecutionHandler(t, model, executor, &sdkconfig.SDKConfig{})
	handler.SetSecretDLP(newSecretDLPTestService(t))

	body, _, errMsg := handler.ExecuteWithAuthManager(secretdlp.WithSession(context.Background(), session), "openai", model, redactedRequest, "")
	if errMsg != nil {
		t.Fatalf("ExecuteWithAuthManager() error = %+v", errMsg)
	}
	if string(body) != fmt.Sprintf(`{"echo":"%s"}`, secret) {
		t.Fatalf("body = %q, want restored secret", body)
	}
}

func TestExecuteStreamWithAuthManagerRestoresSecretDLPStream(t *testing.T) {
	const model = "secret-dlp-stream-model"
	const secret = "sk-testdlpfixture0000000000000000000000000000"

	session, redactedRequest, placeholder := newSecretDLPTestSession(t, model, secret)
	executor := &modelExecutionCaptureExecutor{
		stream: func(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (*coreexecutor.StreamResult, error) {
			chunks := make(chan coreexecutor.StreamChunk, 2)
			split := len(placeholder) / 2
			chunks <- coreexecutor.StreamChunk{Payload: []byte("prefix " + placeholder[:split])}
			chunks <- coreexecutor.StreamChunk{Payload: []byte(placeholder[split:] + " suffix")}
			close(chunks)
			return &coreexecutor.StreamResult{Chunks: chunks}, nil
		},
	}
	handler := newModelExecutionHandler(t, model, executor, &sdkconfig.SDKConfig{})
	handler.SetSecretDLP(newSecretDLPTestService(t))

	dataChan, _, errChan := handler.ExecuteStreamWithAuthManager(secretdlp.WithSession(context.Background(), session), "openai", model, redactedRequest, "")
	var body []byte
	for chunk := range dataChan {
		body = append(body, chunk...)
	}
	for errMsg := range errChan {
		if errMsg != nil {
			t.Fatalf("ExecuteStreamWithAuthManager() error = %+v", errMsg)
		}
	}
	if bytes.Contains(body, []byte(placeholder)) {
		t.Fatalf("stream body still contains placeholder: %q", body)
	}
	if string(body) != "prefix "+secret+" suffix" {
		t.Fatalf("stream body = %q, want restored secret", body)
	}
}

func TestApplySecretDLPAfterAuthHonorsProviderPolicy(t *testing.T) {
	const secret = "sk-testdlpfixture0000000000000000000000000000"

	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, coreauth.NewManager(nil, nil, nil))
	handler.SetSecretDLP(newSecretDLPPolicyTestService(t))
	body := []byte(fmt.Sprintf(`{"messages":[{"role":"user","content":%q}]}`, secret))
	ctx := newSecretDLPGinContext(t, "/v1/chat/completions")

	resp := handler.applySecretDLPAfterAuth(ctx, coreexecutor.RequestAfterAuthInterceptRequest{
		Provider: "example-provider",
		Body:     body,
		Metadata: map[string]any{coreexecutor.RequestPathMetadataKey: "/v1/chat/completions"},
	}, coreexecutor.RequestAfterAuthInterceptResponse{})
	if resp.Err != nil {
		t.Fatalf("applySecretDLPAfterAuth() error: %v", resp.Err)
	}
	if len(resp.Body) == 0 {
		t.Fatal("expected redacted body for enabled provider")
	}
	if bytes.Contains(resp.Body, []byte(secret)) {
		t.Fatalf("redacted body still contains raw secret: %q", resp.Body)
	}
	if !bytes.Contains(resp.Body, []byte("__CPA_DLP_v1_")) {
		t.Fatalf("redacted body missing placeholder: %q", resp.Body)
	}
	placeholder := extractSecretDLPPlaceholder(t, string(resp.Body))
	restored := handler.restoreSecretDLPResponse(ctx, []byte(fmt.Sprintf(`{"echo":"%s"}`, placeholder)))
	if !bytes.Contains(restored, []byte(secret)) {
		t.Fatalf("restored response = %q, want original secret restored", restored)
	}

	disabled := handler.applySecretDLPAfterAuth(context.Background(), coreexecutor.RequestAfterAuthInterceptRequest{
		Provider: "plain-provider",
		Body:     body,
		Metadata: map[string]any{coreexecutor.RequestPathMetadataKey: "/v1/chat/completions"},
	}, coreexecutor.RequestAfterAuthInterceptResponse{})
	if disabled.Err != nil {
		t.Fatalf("disabled provider error: %v", disabled.Err)
	}
	if len(disabled.Body) != 0 {
		t.Fatalf("disabled provider should not mutate body: %q", disabled.Body)
	}

	authOverride := handler.applySecretDLPAfterAuth(context.Background(), coreexecutor.RequestAfterAuthInterceptRequest{
		Provider:              "example-provider",
		SecretRedactionPolicy: "disabled",
		Body:                  body,
		Metadata:              map[string]any{coreexecutor.RequestPathMetadataKey: "/v1/chat/completions"},
	}, coreexecutor.RequestAfterAuthInterceptResponse{})
	if authOverride.Err != nil {
		t.Fatalf("auth override error: %v", authOverride.Err)
	}
	if len(authOverride.Body) != 0 {
		t.Fatalf("auth override should disable body mutation: %q", authOverride.Body)
	}
}

func newSecretDLPTestService(t *testing.T) *secretdlp.Service {
	t.Helper()
	svc, err := secretdlp.New(secretdlp.Config{
		Enabled:        true,
		Mode:           secretdlp.ModeRestore,
		MasterKey:      []byte("master-key"),
		TTL:            time.Minute,
		MaxFindings:    10,
		MinValueLength: 12,
		Scanner:        "builtin",
	})
	if err != nil {
		t.Fatalf("secretdlp.New(): %v", err)
	}
	return svc
}

func newSecretDLPPolicyTestService(t *testing.T) *secretdlp.Service {
	t.Helper()
	svc, err := secretdlp.New(secretdlp.Config{
		Enabled:               true,
		Mode:                  secretdlp.ModeRestore,
		MasterKey:             []byte("master-key"),
		TTL:                   time.Minute,
		MaxFindings:           10,
		MinValueLength:        12,
		Scanner:               "builtin",
		DefaultProviderPolicy: "disabled",
		ProviderOverrides: map[string]string{
			"example-provider": "enabled",
		},
	})
	if err != nil {
		t.Fatalf("secretdlp.New(): %v", err)
	}
	return svc
}

func newSecretDLPGinContext(t *testing.T, path string) context.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, path, nil)
	c.Request.Header.Set("Authorization", "Bearer cliproxy-client-key")
	return context.WithValue(context.Background(), "gin", c)
}

func newSecretDLPTestSession(t *testing.T, model, secret string) (*secretdlp.Session, []byte, string) {
	t.Helper()
	session := secretdlp.NewSession([]byte("master-key"), "client-key", time.Minute, secretdlp.ModeRestore)
	body := []byte(fmt.Sprintf(`{"model":%q,"api_key":%q}`, model, secret))
	redacted, _ := session.RedactRawWithMappings(body, []secretdlp.Finding{{Secret: secret, RuleID: "test", Source: "test"}})
	placeholder := extractSecretDLPPlaceholder(t, string(redacted))
	if strings.Contains(string(redacted), secret) {
		t.Fatalf("redacted request still contains secret: %q", redacted)
	}
	return session, redacted, placeholder
}

func extractSecretDLPPlaceholder(t *testing.T, body string) string {
	t.Helper()
	placeholder := regexp.MustCompile(`__CPA_DLP_v1_[A-Za-z0-9_-]+__`).FindString(body)
	if placeholder == "" {
		t.Fatalf("body %q does not contain DLP placeholder", body)
	}
	return placeholder
}
